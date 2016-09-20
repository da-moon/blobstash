/*

Package blobsfile implement the BlobsFile backend for storing blobs.

It stores multiple blobs (optionally compressed with Snappy) inside "BlobsFile"/fat file/packed file
(256MB by default) (every new file are "fallocate"d).
Blobs are indexed by a kv file.

New blobs are appended to the current file, and when the file exceed the limit, a new fie is created.

Blobs are stored with its hash and its size (for a total overhead of 24 bytes) followed by the blob itself, thus allowing re-indexing.

	Blob hash (32 bytes) + Blob size (4 byte, uint32 binary encoded) + Blob data

Blobs are indexed by a BlobPos entry (value stored as string):

	Blob Hash => n (BlobFile index) + (space) + offset + (space) + Blob size

*/
package blobsfile

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"expvar"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	_ "syscall"

	"github.com/cznic/fileutil"
	"github.com/dchest/blake2b"
	"github.com/fatih/structs"
	"github.com/golang/snappy"
	log2 "gopkg.in/inconshreveable/log15.v2"

	_ "github.com/tsileo/blobstash/pkg/backend"
	"github.com/tsileo/blobstash/pkg/blob"
	"github.com/tsileo/blobstash/pkg/client/clientutil"
	"github.com/tsileo/blobstash/pkg/config/pathutil"
	_ "github.com/tsileo/blobstash/pkg/hashutil"
	"github.com/tsileo/blobstash/pkg/logger"
)

const (
	magic = "\x00Blobs"
)

const (
	defaultMaxBlobsFileSize = 256 << 20 // 256MB
)

const (
	Overhead = 37 // 37 bytes of meta-data are stored for each blob: 32 byte hash + 1 byte flag + 4 byte blob len
	hashSize = 32
)

var (
	openFdsVar      = expvar.NewMap("blobsfile-open-fds")
	bytesUploaded   = expvar.NewMap("blobsfile-bytes-uploaded")
	bytesDownloaded = expvar.NewMap("blobsfile-bytes-downloaded")
	blobsUploaded   = expvar.NewMap("blobsfile-blobs-uploaded")
	blobsDownloaded = expvar.NewMap("blobsfile-blobs-downloaded")
)

type CorruptedError struct {
	blobs []*BlobPos
}

func (ce *CorruptedError) Blobs() []*BlobPos {
	return ce.blobs
}

func (ce *CorruptedError) Error() string {
	return fmt.Sprintf("%d blobs are corrupt", len(ce.blobs))
}

// Blob flags
const (
	Deleted byte = 1 << iota
	Compressed
	Encrypted
	ParityChunk
)

// Config holds the backend config
type Config struct {
	Dir              string `structs:"path,omitempty"`
	Compression      int64  `structs:"compression,omitempty"`
	WriteOnly        bool   `structs:"write-only,omitempty"`
	MaxBlobsFileSize int64  `structs:"blobsfile-max-size,omitempty"`
}

// Backend returns the backend type
func (c *Config) Backend() string {
	return "blobsfile"
}

// Config implements the interface for config
func (c *Config) Config() map[string]interface{} {
	return map[string]interface{}{
		"backend-type": c.Backend(),
		"backend-args": c.Map(),
	}
}

// Map implements the interface for config
func (c *Config) Map() map[string]interface{} {
	return structs.Map(c)
}

// Holds all the backend data
type BlobsFileBackend struct {
	log log2.Logger
	// Directory which holds the blobsfile
	Directory string

	// Maximum size for a blobsfile (256MB by default)
	maxBlobsFileSize int64

	// Backend state
	loaded      bool
	reindexMode bool

	// Compression is disabled by default
	snappyCompression bool

	index *BlobsIndex

	// Current blobs file opened for write
	n       int
	current *os.File
	// Size of the current blobs file
	size int64
	// All blobs files opened for read
	files map[int]*os.File

	wg sync.WaitGroup
	sync.Mutex

	// parityBlobs backend.BlobHandler
	// parityState *parityState
}

// type parityState struct {
// 	nextThresold    int64
// 	lastChunkOffest int64
// }

// New intializes a new BlobsFileBackend
func New(dir string, maxBlobsFileSize int64, compression bool, wg sync.WaitGroup) *BlobsFileBackend {
	dir = strings.Replace(dir, "$VAR", pathutil.VarDir(), -1)
	os.MkdirAll(dir, 0750)
	var reindex bool
	if _, err := os.Stat(filepath.Join(dir, "blobs-index")); os.IsNotExist(err) {
		reindex = true
	}
	index, err := NewIndex(dir)
	if err != nil {
		panic(err)
	}
	if maxBlobsFileSize == 0 {
		maxBlobsFileSize = defaultMaxBlobsFileSize
	}
	backend := &BlobsFileBackend{
		Directory:         dir,
		snappyCompression: compression,
		index:             index,
		files:             make(map[int]*os.File),
		maxBlobsFileSize:  maxBlobsFileSize,
		wg:                wg,
		reindexMode:       reindex,
	}
	backend.log = logger.Log.New("backend", backend.String())
	backend.log.Debug("Started")
	if err := backend.load(); err != nil {
		panic(fmt.Errorf("Error loading %T: %v", backend, err))
	}
	if backend.snappyCompression {
		backend.log.Debug("snappy compression enabled")
	}

	// if !strings.Contains(dir, "blobs-parity") {
	// 	// XXX(tsileo): find a better dir
	// 	parityBackend := New(filepath.Join(pathutil.VarDir(), "blobs-parity"), 0, false, wg)
	// 	backend.parityBlobs = parityBackend
	// 	backend.parityState = &parityState{
	// 		nextThresold: parityChunkSize,
	// 	}
	// }
	return backend
}

// NewFromConfig initialize a BlobsFileBackend from a JSON object.
func NewFromConfig(conf map[string]interface{}, wg sync.WaitGroup) *BlobsFileBackend {
	path := "./backend_blobsfile"
	if _, pathOk := conf["path"]; pathOk {
		path = conf["path"].(string)
	}
	maxsize := 0
	if _, maxsizeOk := conf["blobsfile-max-size"]; maxsizeOk {
		maxsize = conf["blobsfile-max-size"].(int)
	}
	compression := false
	if _, cOk := conf["compression"]; cOk {
		compression = conf["compression"].(bool)
	}
	return New(path, int64(maxsize), compression, wg)
}

// Len compute the number of blobs stored
func (backend *BlobsFileBackend) Len() int {
	storedBlobs := make(chan string)
	go backend.Enumerate(storedBlobs)
	cnt := 0
	for _ = range storedBlobs {
		cnt++
	}
	return cnt
}

func (backend *BlobsFileBackend) IterOpenFiles() (files []*os.File) {
	for _, f := range backend.files {
		files = append(files, f)
	}
	return files
}

func (backend *BlobsFileBackend) CloseOpenFiles() {
	for _, f := range backend.files {
		f.Close()
	}
}

func (backend *BlobsFileBackend) Close() {
	backend.log.Debug("closing index...")
	// if backend.parityBlobs != nil {
	// 	backend.parityBlobs.Close()
	// }
	backend.index.Close()
}

// TODO(tsileo): remove this fron the Backend interface
func (backend *BlobsFileBackend) Done() error {
	return nil
}

// Remove the index
func (backend *BlobsFileBackend) Remove() {
	backend.index.Remove()
}

// GetN returns the total numbers of BlobsFile
func (backend *BlobsFileBackend) GetN() (int, error) {
	return backend.index.GetN()
}

func (backend *BlobsFileBackend) saveN() error {
	return backend.index.SetN(backend.n)
}

func (backend *BlobsFileBackend) restoreN() error {
	n, err := backend.index.GetN()
	if err != nil {
		return err
	}
	backend.n = n
	return nil
}

// Implements the Stringer interface
func (backend *BlobsFileBackend) String() string {
	return fmt.Sprintf("blobsfile-%v", backend.Directory)
}

func (backend *BlobsFileBackend) scan(iterFunc func(*BlobPos, byte, string, []byte) error) error {
	n := 0
	corrupted := []*BlobPos{}
	for {
		err := backend.ropen(n)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return err
		}
		offset := 6
		blobsfile := backend.files[n]
		blobsIndexed := 0
		for {
			// SCAN
			blobHash := make([]byte, hashSize)
			blobSizeEncoded := make([]byte, 4)
			flags := make([]byte, 1)
			if _, err := blobsfile.Read(blobHash); err == io.EOF {
				break
			}
			if _, err := blobsfile.Read(flags); err != nil {
				return err
			}
			if _, err := blobsfile.Read(blobSizeEncoded); err != nil {
				return err
			}
			blobSize := binary.LittleEndian.Uint32(blobSizeEncoded)
			rawBlob := make([]byte, int(blobSize))
			read, err := blobsfile.Read(rawBlob)
			if err != nil || read != int(blobSize) {
				return fmt.Errorf("error while reading raw blob: %v", err)
			}
			if flags[0] == Deleted {
				backend.log.Debug("blob deleted, continue indexing")
				offset += Overhead + int(blobSize)
				continue
			}
			blobPos := &BlobPos{n: n, offset: offset, size: int(blobSize)}
			offset += Overhead + int(blobSize)
			var blob []byte
			if backend.snappyCompression {
				blobDecoded, err := snappy.Decode(nil, rawBlob)
				if err != nil {
					return fmt.Errorf("failed to decode blob: %v %v %v", err, blobSize, flags)
				}
				blob = blobDecoded
			} else {
				blob = rawBlob
			}
			hash := fmt.Sprintf("%x", blake2b.Sum256(blob))
			if fmt.Sprintf("%x", blobHash) == hash {
				if err := iterFunc(blobPos, flags[0], hash, blob); err != nil {
					return err
				}
				blobsIndexed++
				// FIXME(tsileo): continue an try to repair it?
			} else {
				// better out an error and provides a CLI for repairing
				backend.log.Error(fmt.Sprintf("hash doesn't match %v/%v", fmt.Sprintf("%x", blobHash), hash))
				corrupted = append(corrupted, blobPos)
			}
		}
		log.Printf("BlobsFileBackend: %v iter (%v blobs)", backend.filename(n), blobsIndexed)
		n++
	}
	if len(corrupted) > 0 {
		return &CorruptedError{corrupted}
	}
	if n == 0 {
		backend.log.Debug("no BlobsFiles found for re-indexing")
		return nil
	}
	return nil
}

// reindex scans all BlobsFile and reconstruct the index from scratch.
func (backend *BlobsFileBackend) reindex() error {
	backend.wg.Add(1)
	defer backend.wg.Done()
	backend.log.Info("re-indexing BlobsFiles...")
	if backend.Len() != 0 {
		panic("can't re-index, an non-empty backend already exists")
	}
	n := 0
	blobsIndexed := 0

	iterFunc := func(blobPos *BlobPos, _ byte, hash string, _ []byte) error {
		if err := backend.index.SetPos(hash, blobPos); err != nil {
			return err
		}
		n = blobPos.n
		blobsIndexed++
		return nil
	}

	if err := backend.scan(iterFunc); err != nil {
		return err
	}

	// FIXME(tsileo): check for CorruptedError and initialize a repair

	if n == 0 {
		backend.log.Debug("no BlobsFiles found for re-indexing")
		return nil
	}
	if err := backend.saveN(); err != nil {
		return err
	}
	return nil
}

// Open all the blobs-XXXXX (read-only) and open the last for write
func (backend *BlobsFileBackend) load() error {
	backend.wg.Add(1)
	defer backend.wg.Done()
	backend.log.Debug("BlobsFileBackend: scanning BlobsFiles...")
	n := 0
	for {
		err := backend.ropen(n)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return err
		}
		backend.log.Debug("BlobsFile loaded", "name", backend.filename(n))
		n++
	}
	if n == 0 {
		// The dir is empty, create a new blobs-XXXXX file,
		// and open it for read
		if err := backend.wopen(n); err != nil {
			return err
		}
		if err := backend.ropen(n); err != nil {
			return err
		}
		if err := backend.saveN(); err != nil {
			return err
		}
		backend.loaded = true
		return nil
	}
	// Open the last file for write
	if err := backend.wopen(n - 1); err != nil {
		return err
	}
	if err := backend.saveN(); err != nil {
		return err
	}
	backend.loaded = true
	if backend.reindexMode {
		if err := backend.reindex(); err != nil {
			return err
		}
	}
	return nil
}

// Open a file for writing, will close the previously open file if any.
func (backend *BlobsFileBackend) wopen(n int) error {
	backend.log.Info("opening blobsfile for writing", "name", backend.filename(n))
	// Close the already opened file if any
	if backend.current != nil {
		if err := backend.current.Close(); err != nil {
			openFdsVar.Add(backend.Directory, -1)
			return err
		}
	}
	created := false
	if _, err := os.Stat(backend.filename(n)); os.IsNotExist(err) {
		created = true
	}
	f, err := os.OpenFile(backend.filename(n), os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	backend.current = f
	backend.n = n
	if created {
		if ferr := backend.allocateBlobsFile(); ferr != nil {
			log.Printf("BlobsFileBackend: fallocate file %v error: %v", backend.filename(n), ferr)
		}
		// Write the header/magic number
		_, err := backend.current.Write([]byte(magic))
		if err != nil {
			return err
		}
		if err = backend.current.Sync(); err != nil {
			panic(err)
		}
	}
	backend.size, err = f.Seek(0, os.SEEK_END)
	if err != nil {
		return err
	}
	openFdsVar.Add(backend.Directory, 1)
	return nil
}

// Open a file for read
func (backend *BlobsFileBackend) ropen(n int) error {
	_, alreadyOpen := backend.files[n]
	if alreadyOpen {
		log.Printf("BlobsFileBackend: blobsfile %v already open", backend.filename(n))
		return nil
	}
	if n > len(backend.files) {
		return fmt.Errorf("Trying to open file %v whereas only %v files currently open", n, len(backend.files))
	}

	filename := backend.filename(n)
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	fmagic := make([]byte, len(magic))
	_, err = f.Read(fmagic)
	if err != nil || magic != string(fmagic) {
		return fmt.Errorf("magic not found in BlobsFile")
	}
	backend.files[n] = f
	openFdsVar.Add(backend.Directory, 1)
	return nil
}

// Generate a new blobs file and fallocate a 256MB file.
func (backend *BlobsFileBackend) allocateBlobsFile() error {
	log.Printf("BlobsFileBackend: running fallocate on BlobsFile %v", backend.filename(backend.n))
	// fallocate 256MB
	//if err := syscall.Fallocate(int(backend.current.Fd()), 0x01, 0, backend.maxBlobsFileSize); err != nil {
	//	return err
	//}
	// TODO check
	return nil
}

func (backend *BlobsFileBackend) filename(n int) string {
	return filepath.Join(backend.Directory, fmt.Sprintf("blobs-%05d", n))
}

// Put save a new blob
func (backend *BlobsFileBackend) Put(hash string, data []byte) (err error) {
	if !backend.loaded {
		panic("backend BlobsFileBackend not loaded")
	}
	// Acquire the lock
	backend.Lock()
	defer backend.Unlock()

	// Encode the blob
	blobSize, blobEncoded := backend.encodeBlob(data)

	// Ensure the blosfile size won't exceed the maxBlobsFileSize
	if backend.size+int64(blobSize) > backend.maxBlobsFileSize {
		// Archive this blobsfile, start by creating a new one
		backend.n++
		backend.log.Debug("creating a new BlobsFile")
		if err := backend.wopen(backend.n); err != nil {
			panic(err)
		}
		// Re-open it (since we may need to read blobs from it)
		if err := backend.ropen(backend.n); err != nil {
			panic(err)
		}
		// Update the nimber of blobsfile in the index
		if err := backend.saveN(); err != nil {
			panic(err)
		}
	}

	// Save the blob in the index
	blobPos := &BlobPos{n: backend.n, offset: int(backend.size), size: blobSize}
	if err := backend.index.SetPos(hash, blobPos); err != nil {
		return err
	}

	// Actually save the blob
	n, err := backend.current.Write(blobEncoded)
	backend.size += int64(len(blobEncoded))
	if err != nil || n != len(blobEncoded) {
		return fmt.Errorf("Error writing blob (%v,%v)", err, n)
	}

	// Flush the backend
	if err = backend.current.Sync(); err != nil {
		panic(err)
	}

	// Check if we need to compute parity blocks
	// FIXME(tsileo): extract this, and run it before creating a new blobsfile (with paddin if it's the last)
	// XXX(tsileo): also, when ran at the end, it should also read all the parity blobs in the right order, and
	// save it at the end of the current blobsfile
	// var lastRun bool
	// if !strings.Contains(backend.Directory, "parity") && backend.size >= backend.parityState.nextThresold {
	// 	// FIXME(tsileo): handdle the thresold

	// 	if _, err := backend.current.Seek(((backend.parityState.nextThresold/parityChunkSize)-1)*parityChunkSize, os.SEEK_SET); err != nil {
	// 		panic(err)
	// 	}
	// 	data := make([]byte, parityChunkSize)
	// 	if _, err := backend.current.Read(data); err != nil {
	// 		panic(err)
	// 	}
	// 	parityBlobs := [][]byte{}
	// 	// FIXME(tsileo): compute the parity blobs and handling of chunk in the init
	// 	for _, blob := range parityBlobs {
	// 		hash := hashutil.Compute(blob)
	// 		if err := backend.parityBlobs.Put(hash, blob); err != nil {
	// 			panic(err)
	// 		}
	// 	}
	// 	backend.parityState.nextThresold = backend.parityState.nextThresold * 2
	// 	if _, err := backend.current.Seek(backend.size, os.SEEK_SET); err != nil {
	// 		panic(err)
	// 	}
	// 	if lastRun {
	// 		iterFunc := func(blobPos *BlobPos, _ byte, hash string, data []byte) error {
	// 			// FIXME(tsileo): be able
	// 			// backend.Put(hash, data) FlagParityChunk
	// 			return nil
	// 		}
	// 		if err := backend.parityBlobs.(*BlobsFileBackend).scan(iterFunc); err != nil {
	// 			return err
	// 		}
	// 		// TODO(tsileo): iter parityBlobs and save the blobs in the backend,
	// 		// and close it
	// 		// and reset the parityBlobs
	// 	}
	// }

	// Update the expvars
	bytesUploaded.Add(backend.Directory, int64(len(blobEncoded)))
	blobsUploaded.Add(backend.Directory, 1)
	return
}

// Alias for exists
func (backend *BlobsFileBackend) Stat(hash string) (bool, error) {
	return backend.Exists(hash)
}

// Exists check if a blob is present
func (backend *BlobsFileBackend) Exists(hash string) (bool, error) {
	blobPos, err := backend.index.GetPos(hash)
	if err != nil {
		return false, err
	}
	if blobPos != nil {
		return true, nil
	}
	return false, nil
}

func (backend *BlobsFileBackend) decodeBlob(data []byte) (size int, blob []byte) {
	//flag := data[hashSize]
	size = int(binary.LittleEndian.Uint32(data[hashSize+1 : Overhead]))
	blob = make([]byte, size)
	copy(blob, data[Overhead:])
	if backend.snappyCompression {
		blobDecoded, err := snappy.Decode(nil, blob)
		if err != nil {
			panic(fmt.Errorf("Failed to decode blob with Snappy: %v", err))
		}
		blob = blobDecoded
	}
	h := blake2b.New256()
	h.Write(blob)
	if !bytes.Equal(h.Sum(nil), data[0:hashSize]) {
		panic(fmt.Errorf("Hash doesn't match %x != %x", h.Sum(nil), data[0:hashSize]))
	}
	return
}

func (backend *BlobsFileBackend) encodeBlob(blob []byte) (size int, data []byte) {
	h := blake2b.New256()
	h.Write(blob)

	if backend.snappyCompression {
		dataEncoded := snappy.Encode(nil, blob)
		blob = dataEncoded
	}
	size = len(blob)
	data = make([]byte, len(blob)+Overhead)
	copy(data[:], h.Sum(nil))
	// set the flag
	data[hashSize] = 0
	binary.LittleEndian.PutUint32(data[hashSize+1:], uint32(size))
	copy(data[Overhead:], blob)
	return
}

// BlobPos return the index entry for the given hash
func (backend *BlobsFileBackend) BlobPos(hash string) (*BlobPos, error) {
	return backend.index.GetPos(hash)
}

// Get returns the blob fur the given hash
func (backend *BlobsFileBackend) Get(hash string) ([]byte, error) {
	if !backend.loaded {
		panic("backend BlobsFileBackend not loaded")
	}

	// Fetch the index entry
	blobPos, err := backend.index.GetPos(hash)
	if err != nil {
		return nil, fmt.Errorf("Error fetching GetPos: %v", err)
	}

	// No index entry found, returns an error
	if blobPos == nil {
		if err == nil {
			return nil, clientutil.ErrBlobNotFound
		} else {
			return nil, err
		}
	}

	// Read the encoded blob from the BlobsFile
	data := make([]byte, blobPos.size+Overhead)
	n, err := backend.files[blobPos.n].ReadAt(data, int64(blobPos.offset))
	if err != nil {
		return nil, fmt.Errorf("Error reading blob: %v / blobsfile: %+v", err, backend.files[blobPos.n])
	}

	// Ensure the data lenght is expcted
	if n != blobPos.size+Overhead {
		return nil, fmt.Errorf("Error reading blob %v, read %v, expected %v+%v", hash, n, blobPos.size, Overhead)
	}

	// Decode the blob
	blobSize, blob := backend.decodeBlob(data)
	if blobSize != blobPos.size {
		return nil, fmt.Errorf("Bad blob %v encoded size, got %v, expected %v", hash, n, blobSize)
	}

	// Update the expvars
	bytesDownloaded.Add(backend.Directory, int64(blobSize))
	blobsUploaded.Add(backend.Directory, 1)

	return blob, nil
}

// Delete a blob (by setting the `Deleted` flag)
func (backend *BlobsFileBackend) Delete(hash string) error {
	if !backend.loaded {
		panic("backend BlobsFileBackend not loaded")
	}

	blobPos, err := backend.index.GetPos(hash)
	if err != nil {
		return fmt.Errorf("Error fetching GetPos: %v", err)
	}
	if blobPos == nil {
		return fmt.Errorf("Blob %v not found in index", err)
	}
	var f *os.File
	// check if the file is already open for writing
	if blobPos.n == backend.n {
		f = backend.current
	} else {
		f, err = os.OpenFile(backend.filename(blobPos.n), os.O_RDWR, 0666)
		if err != nil {
			return fmt.Errorf("failed to open blobsfile %v", backend.filename(blobPos.n), err)
		}
		defer f.Close()
	}
	// Add Deleted to the flag
	if _, err := f.WriteAt([]byte{Deleted}, int64(blobPos.offset+hashSize)); err != nil {
		return err
	}
	// Delete the index entry
	if err := backend.index.DeletePos(hash); err != nil {
		return err
	}
	// Punch a hole in the file if possible
	if err := fileutil.PunchHole(f, int64(blobPos.offset+Overhead), int64(blobPos.size)); err != nil {
		return fmt.Errorf("failed to punch hole: %v", err)
	}
	return nil
}

// Enumerate output all the blobs hashes in the given chan
func (backend *BlobsFileBackend) Enumerate(blobs chan<- string) error {
	defer close(blobs)
	if !backend.loaded {
		panic("backend BlobsFileBackend not loaded")
	}
	backend.Lock()
	defer backend.Unlock()
	// TODO(tsileo) send the size along the hashes ?
	enum, _, err := backend.index.db.Seek(formatKey(BlobPosKey, []byte("")))
	if err != nil {
		return err
	}
	for {
		k, _, err := enum.Next()
		if err == io.EOF {
			break
		}
		// Remove the BlobPosKey prefix byte
		blobs <- hex.EncodeToString(k[1:])
	}
	return nil
}

// Enumerate output all the blobs into the given chan (ordered lexicographically)
func (backend *BlobsFileBackend) Enumerate2(blobs chan<- *blob.SizedBlobRef, start, end string, limit int) error {
	defer close(blobs)
	if !backend.loaded {
		panic("backend BlobsFileBackend not loaded")
	}
	backend.Lock()
	defer backend.Unlock()
	// TODO(tsileo) send the size along the hashes ?
	// fmt.Printf("start=%v/%+v\n", start, formatKey(BlobPosKey, []byte(start)))
	s, err := hex.DecodeString(start)
	if err != nil {
		return err
	}
	enum, _, err := backend.index.db.Seek(formatKey(BlobPosKey, s))
	// endBytes := formatKey(BlobPosKey, []byte(end))
	endBytes := []byte(end)
	// formatKey(BlobPosKey, []byte(end))
	if err != nil {
		return err
	}
	i := 0
	for {
		k, _, err := enum.Next()
		if err == io.EOF {
			break
		}
		// FIXME(tsileo): fix this mess
		hash := hex.EncodeToString(k[1:])
		// fmt.Printf("%+v/%+v/%+v\n", k, endBytes, bytes.Compare(k, endBytes))
		// fmt.Printf("%+v/%+v/%+v\n", strings.Compare(hash, string(endBytes[1:])), hash, endBytes[1:])
		if bytes.Compare(k, endBytes) > 0 || (limit != 0 && i > limit) {
			return nil
		}
		blobPos, err := backend.BlobPos(hash)
		if err != nil {
			return nil
		}
		// Remove the BlobPosKey prefix byte
		sbr := &blob.SizedBlobRef{
			Hash: hex.EncodeToString(k[1:]),
			Size: blobPos.size,
		}
		blobs <- sbr
		i++
	}
	return nil
}
