package filetree

import (
	"encoding/json"
	_ "encoding/json"
	_ "encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/mux"
	log "github.com/inconshreveable/log15"
	"golang.org/x/net/context"

	"github.com/tsileo/blobstash/pkg/blob"
	"github.com/tsileo/blobstash/pkg/blobstore"
	"github.com/tsileo/blobstash/pkg/client/clientutil"
	"github.com/tsileo/blobstash/pkg/config"
	"github.com/tsileo/blobstash/pkg/filetree/filetreeutil/meta"
	"github.com/tsileo/blobstash/pkg/filetree/reader/filereader"
	"github.com/tsileo/blobstash/pkg/filetree/writer"
	"github.com/tsileo/blobstash/pkg/httputil"
	"github.com/tsileo/blobstash/pkg/httputil/bewit"
	"github.com/tsileo/blobstash/pkg/httputil/resize"
	"github.com/tsileo/blobstash/pkg/kvstore"
	"github.com/tsileo/blobstash/pkg/vkv"
)

// XXX(tsileo): handle the fetching of meta from the FS name and reconstruct the vkv key
// to the root in children link
// XXX(tsileo): bind a folder to the root path, e.g. {hostname}/ => dirHandler?
// XXX(tsileo): a multi-part upload endpoint (but without dir capabilities, at least for now)

var (
	indexFile = "index.html"

	FSKeyFmt = "_:filetree:fs:%s"
	// PermName     = "filetree"
	// PermTreeName = "filetree:root:"
	// PermWrite    = "write"
	// PermRead     = "read"

	MaxUploadSize int64 = 32 << 20
)

type FileTreeExt struct {
	kvStore     *kvstore.KvStore
	blobStore   *blobstore.BlobStore
	conf        *config.Config
	sharingCred *bewit.Cred
	authFunc    func(*http.Request) bool
	shareTTL    time.Duration

	log log.Logger
}

// BlobStore is the interface to be compatible with both the server and the BlobStore client
type BlobStore struct {
	blobStore *blobstore.BlobStore
}

func (bs *BlobStore) Get(hash string) ([]byte, error) {
	return bs.blobStore.Get(context.TODO(), hash)
}

func (bs *BlobStore) Stat(hash string) (bool, error) {
	return bs.blobStore.Stat(context.TODO(), hash)
}

func (bs *BlobStore) Put(hash string, data []byte) error {
	return bs.blobStore.Put(context.TODO(), &blob.Blob{Hash: hash, Data: data})
}

type FS struct {
	Name string `json:"-"`
	Ref  string `json:"ref"`

	ft *FileTreeExt `json:"-"`
}

// New initializes the `DocStoreExt`
func New(logger log.Logger, conf *config.Config, authFunc func(*http.Request) bool, kvStore *kvstore.KvStore, blobStore *blobstore.BlobStore) (*FileTreeExt, error) {
	logger.Debug("init")
	return &FileTreeExt{
		conf:      conf,
		kvStore:   kvStore,
		blobStore: blobStore,
		sharingCred: &bewit.Cred{
			Key: []byte(conf.SharingKey),
			ID:  "filetree",
		},
		authFunc: authFunc,
		shareTTL: 1 * time.Hour,
		log:      logger,
	}, nil
}

// Close closes all the open DB files.
func (ft *FileTreeExt) Close() error {
	return nil
}

// RegisterRoute registers all the HTTP handlers for the extension
func (ft *FileTreeExt) Register(r *mux.Router, root *mux.Router, basicAuth func(http.Handler) http.Handler) {
	// Raw node endpoint
	r.Handle("/node/{ref}", basicAuth(http.HandlerFunc(ft.nodeHandler())))

	// Public/semi-private handler
	dirHandler := http.HandlerFunc(ft.dirHandler())
	fileHandler := http.HandlerFunc(ft.fileHandler())

	r.Handle("/fs/{name}", http.HandlerFunc(ft.fsHandler()))
	r.Handle("/fs/{name}/{path:.+}", http.HandlerFunc(ft.fsHandler()))
	// r.Handle("/fs", http.HandlerFunc(ft.fsHandler()))
	// r.Handle("/fs/{name}", http.HandlerFunc(ft.fsByNameHandler()))

	r.Handle("/upload", http.HandlerFunc(ft.uploadHandler()))

	// Hook the standard endpint
	r.Handle("/dir/{ref}", dirHandler)
	r.Handle("/file/{ref}", fileHandler)

	// Enable shortcut path from the root
	root.Handle("/d/{ref}", dirHandler)
	root.Handle("/f/{ref}", fileHandler)
}

// Node holds the data about the file node (either file/dir), analog to a Meta
type Node struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Size     int     `json:"size"`
	Mode     uint32  `json:"mode"`
	ModTime  string  `json:"mtime"`
	Hash     string  `json:"ref"`
	Children []*Node `json:"children,omitempty"`

	Extra  map[string]interface{} `json:"extra,omitempty"`
	XAttrs map[string]string      `json:"xattrs,omitempty"`

	meta   *meta.Meta `json:"-"`
	parent *Node      `json:"-"`
	fs     *FS        `json:"-"`
}

// Update the given node with the given meta
func (ft *FileTreeExt) Update(n *Node, m *meta.Meta) (*Node, error) {
	newNode, err := metaToNode(m)
	if err != nil {
		return nil, err
	}
	newNode.fs = n.fs
	newNode.parent = n.parent
	fmt.Printf("\n\n\n###Update: n=%+v\nn.meta=%+v\nn.parent=%+v\nm=%+v\nnewNode=%+v\n\n\n###", n, n.meta, n.parent, m, newNode)
	if n.parent != nil {
		fmt.Printf("n.parent.meta=%+v\n", n.parent.meta)

	}
	if n.parent == nil {
		n.fs.Ref = newNode.Hash
		js, err := json.Marshal(n.fs)
		if err != nil {
			return nil, err
		}
		if ft.kvStore.Put(context.TODO(), fmt.Sprintf(FSKeyFmt, n.fs.Name), "", js, -1); err != nil {
			return nil, err
		}
		return newNode, nil
	}

	newRefs := []interface{}{newNode.Hash}
	newChildren := []*Node{newNode}

	for _, c := range newNode.parent.Children {
		if c.Hash != n.Hash {
			newRefs = append(newRefs, c.Hash)
			newChildren = append(newChildren, c)
		}
	}

	newNode.parent.meta.Refs = newRefs
	newNode.parent.Children = newChildren

	// parentMeta := n.parent.meta
	// parentMeta.Refs = newRefs
	// n.parent.Children = newChildren

	// Update the node  modtime
	newNode.parent.meta.ModTime = time.Now().Format(meta.ModTimeFmt)
	fmt.Printf("saving parent meta: %+v\n", newNode.parent.meta)
	newRef, data := newNode.parent.meta.Json()
	newNode.parent.Hash = newRef
	newNode.parent.meta.Hash = newRef
	if err := ft.blobStore.Put(context.TODO(), &blob.Blob{Hash: newRef, Data: data}); err != nil {
		return nil, err
	}
	// n.parent.Hash = newRef
	// parentMeta.Hash = newRef
	// Propagate the change to the parents
	if _, err := ft.Update(newNode.parent, newNode.parent.meta); err != nil {
		return nil, err
	}
	return newNode, nil
}

func (n *Node) Close() error {
	// FIXME(tsileo): no nore Meta pool
	n.meta.Close()
	return nil
}

type byName []*Node

func (s byName) Len() int           { return len(s) }
func (s byName) Less(i, j int) bool { return s[i].Name < s[j].Name }
func (s byName) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

func metaToNode(m *meta.Meta) (*Node, error) {
	return &Node{
		Name:    m.Name,
		Type:    m.Type,
		Size:    m.Size,
		Mode:    m.Mode,
		ModTime: m.ModTime,
		Extra:   m.Extra,
		XAttrs:  m.XAttrs,
		Hash:    m.Hash,
		meta:    m,
	}, nil
}

// fetchDir recursively fetch dir children
func (ft *FileTreeExt) fetchDir(n *Node, depth, maxDepth int) error {
	if depth > maxDepth {
		return nil
	}
	if n.Type == "dir" {
		n.Children = []*Node{}
		for _, ref := range n.meta.Refs {
			cn, err := ft.nodeByRef(ref.(string))
			if err != nil {
				return err
			}
			n.Children = append(n.Children, cn)
			if err := ft.fetchDir(cn, depth+1, maxDepth); err != nil {
				return err
			}
		}
	}
	// n.meta.Close()
	return nil
}

// FS fetch the FileSystem by name, returns an empty one if not found
func (ft *FileTreeExt) FS(name string) (*FS, error) {
	fs := &FS{}
	kv, err := ft.kvStore.Get(context.TODO(), fmt.Sprintf(FSKeyFmt, name), -1)
	if err != nil && err != vkv.ErrNotFound {
		return nil, err
	}
	switch err {
	case nil:
		if err := json.Unmarshal([]byte(kv.Data), fs); err != nil {
			return nil, err
		}
	case vkv.ErrNotFound:
		// XXX(tsileo): should the `ErrNotFound` be returned here?
	default:
		return nil, err
	}
	fs.Name = name
	fs.ft = ft
	return fs, nil
}

// Root fetch the FS root, and creates a new one if `create` is set to true (but it won't be savec automatically in the BlobStore
func (fs *FS) Root(create bool) (*Node, error) {
	node, err := fs.ft.nodeByRef(fs.Ref)
	switch err {
	case clientutil.ErrBlobNotFound:
		if !create {
			return nil, err
		}
		meta := meta.NewMeta()
		meta.Type = "dir"
		meta.Name = "_root"
		node, err = metaToNode(meta)
		if err != nil {
			return nil, err
		}
	case nil:
	default:
		return nil, err
	}
	node.fs = fs
	return node, nil
}

// Path returns the `Node` at the given path, create it if requested
func (fs *FS) Path(path string, create bool) (*Node, error) {
	node, err := fs.Root(create)
	if err != nil {
		return nil, err
	}
	var prev *Node
	node.fs = fs
	node.parent = nil
	if err := fs.ft.fetchDir(node, 1, 1); err != nil {
		return nil, err
	}
	if path == "/" {
		return node, nil
	}
	split := strings.Split(path[1:], "/")
	// fmt.Printf("split res=%+v\n", split)
	// Split the path, and fetch each node till the last one
	pathCount := len(split)
	for i, p := range split {
		prev = node
		found := false
		// fmt.Printf("split:%+v\n", p)
		for _, child := range node.Children {
			if child.Name == p {
				node, err = fs.ft.nodeByRef(child.Hash)
				if err != nil {
					return nil, err
				}
				if err := fs.ft.fetchDir(node, 1, 1); err != nil {
					return nil, err
				}
				node.parent = prev
				node.fs = fs
				// fmt.Printf("split:%+v fetched:%+v\n", p, node)
				found = true
				break
			}
		}
		// fmt.Printf("split:%+v, node=%+v\n", p, node)
		// At this point, we found no node at the given path
		if !found {
			if !create {
				return nil, clientutil.ErrBlobNotFound
			}
			// Create a new dir since it doesn't exist
			meta := meta.NewMeta()
			meta.Name = p
			meta.Type = "dir"
			if i == pathCount-1 {
				meta.Type = "file"
			}
			//  we don't set the meta type, it will be set on Update if it doesn't exist
			node, err = metaToNode(meta)
			if err != nil {
				return nil, err
			}
			node.parent = prev
			node.fs = fs
			// fmt.Printf("split:%+v created:%+v\n", p, node)
		}
	}
	return node, nil
}

// Handle multipart form upload to create a new Node (outside of any FS)
func (ft *FileTreeExt) uploadHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		r.ParseMultipartForm(MaxUploadSize)
		file, handler, err := r.FormFile("file")
		if err != nil {
			panic(err)
		}
		defer file.Close()
		uploader := writer.NewUploader(&BlobStore{ft.blobStore})
		meta, err := uploader.PutReader(handler.Filename, file)
		if err != nil {
			panic(err)
		}
		node, err := metaToNode(meta)
		if err != nil {
			panic(err)
		}
		httputil.WriteJSON(w, node)
	}
}

func (ft *FileTreeExt) fsHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		fsName := vars["name"]
		path := "/" + vars["path"]

		fs, err := ft.FS(fsName)
		if err != nil {
			panic(err)
		}
		switch r.Method {
		case "GET", "HEAD":
			node, err := fs.Path(path, false)
			switch err {
			case clientutil.ErrBlobNotFound:
				// Returns a 404 if the blob/children is not found
				w.WriteHeader(http.StatusNotFound)
				return
			default:
				panic(err)
			}

			// Handle HEAD request
			if r.Method == "HEAD" {
				return
			}

			// Returns the Node as JSON
			httputil.WriteJSON(w, node)
		case "POST":
			// FIXME(tsileo): add a way to upload a file as public ? like AWS S3 public-read canned ACL
			// Add a new node in the FS at the given path
			node, err := fs.Path(path, true)
			if err != nil {
				panic(err)
			}
			// fmt.Printf("Current node:%v %+v %+v\n", path, node, node.meta)
			// fmt.Printf("Current node parent:%+v %+v\n", node.parent, node.parent.meta)
			r.ParseMultipartForm(MaxUploadSize)
			file, _, err := r.FormFile("file")
			if err != nil {
				panic(err)
			}
			defer file.Close()
			uploader := writer.NewUploader(&BlobStore{ft.blobStore})

			// Create/save me Meta
			meta, err := uploader.PutReader(filepath.Base(path), file)
			if err != nil {
				panic(err)
			}

			// Update the Node with the new Meta
			// fmt.Printf("uploaded meta=%+v\nold node=%+v", meta, node)
			newNode, err := ft.Update(node, meta)
			if err != nil {
				panic(err)
			}

			httputil.WriteJSON(w, newNode)
		// FIXME(tsileo): handle delete
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

	}
}

// func (ft *FileTreeExt) fsHandler() func(http.ResponseWriter, *http.Request) {
// 	return func(w http.ResponseWriter, r *http.Request) {
// 		// Check permissions
// 		// permissions.CheckPerms(r, PermName)

// 		// List available FS, and display them
// 		switch r.Method {
// 		case "GET", "HEAD":
// 			if r.Method == "HEAD" {
// 				return
// 			}
// 			res, err := ft.kvStore.Keys(context.TODO(), "filetree:fs:", "filetree:fs:\xff", 0)
// 			if err != nil {
// 				panic(err)
// 			}
// 			out := []*fsmod.FS{}
// 			for _, kv := range res {
// 				fs := &fsmod.FS{}
// 				if err := json.Unmarshal([]byte(kv.Value), fs); err != nil {
// 					panic(err)
// 				}
// 				out = append(out, fs)
// 			}
// 			httputil.WriteJSON(w, out)
// 		case "POST":
// 			defer r.Body.Close()
// 			fs := &fsmod.FS{}
// 			if err := json.NewDecoder(r.Body).Decode(fs); err != nil {
// 				panic(err)
// 			}
// 			fs.SetDB(ft.kvStore)
// 			if err := fs.Mutate(fs.Ref); err != nil {
// 				panic(err)
// 			}
// 			httputil.WriteJSON(w, fs)
// 		default:
// 			w.WriteHeader(http.StatusMethodNotAllowed)
// 			return
// 		}
// 	}
// }

// func (ft *FileTreeExt) fsByNameHandler() func(http.ResponseWriter, *http.Request) {
// 	return func(w http.ResponseWriter, r *http.Request) {
// 		// Check permissions
// 		// permissions.CheckPerms(r, PermName)
// 		vars := mux.Vars(r)

// 		name := vars["name"]
// 		fs, err := ft.loadFS(name)
// 		if err != nil {
// 			panic(err)
// 		}

// 		// Display a single FS and handle mutation via POST
// 		switch r.Method {
// 		case "GET", "HEAD":
// 			if r.Method == "HEAD" {
// 				return
// 			}
// 			httputil.WriteJSON(w, fs)
// 		case "POST":
// 			defer r.Body.Close()
// 			fsUpdate := &fsmod.FS{}
// 			if err := json.NewDecoder(r.Body).Decode(fsUpdate); err != nil {
// 				panic(err)
// 			}
// 			// FIXME(tsileo): handle `Data` field mutation
// 			fs.SetDB(ft.kvStore)
// 			if err := fs.Mutate(fsUpdate.Ref); err != nil {
// 				panic(err)
// 			}

// 		default:
// 			w.WriteHeader(http.StatusMethodNotAllowed)
// 			return
// 		}
// 	}
// }

// fileHandler serve the Meta like it's a standard file
func (ft *FileTreeExt) fileHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		vars := mux.Vars(r)

		hash := vars["ref"]
		ft.serveFile(w, r, hash)
	}
}

// isPublic ensures the givem Meta is public
func (ft *FileTreeExt) isPublic(m *meta.Meta) (bool, error) {
	if m.IsPublic() {
		ft.log.Debug("XAttrs public=1")
		return true, nil
	}
	// FIXME(tsileo): a way to find if a meta is part of a public directory
	return false, nil
}

// serveFile serve the node as a file using `net/http` FS util
func (ft *FileTreeExt) serveFile(w http.ResponseWriter, r *http.Request, hash string) {
	var authorized bool

	if err := bewit.Validate(r, ft.sharingCred); err != nil {
		ft.log.Debug("invalid bewit", "err", err)
	} else {
		ft.log.Debug("valid bewit")
		authorized = true
	}

	blob, err := ft.blobStore.Get(context.TODO(), hash)
	if err != nil {
		if err == clientutil.ErrBlobNotFound {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		panic(err)
	}

	m, err := meta.NewMetaFromBlob(hash, blob)
	if err != nil {
		panic(err)
	}
	defer m.Close()

	if !authorized && m.IsPublic() {
		ft.log.Debug("XAttrs public=1")
		authorized = true
	}

	if !authorized {
		// Try if an API key is provided
		ft.log.Info("before authFunc")
		if !ft.authFunc(r) {
			// Rreturns a 404 to prevent leak of hashes
			notFound(w)
			return
		}
	}

	if m.IsDir() {
		panic(httputil.NewPublicErrorFmt("node is not a file (%s)", m.Type))
	}

	// Initialize a new `File`
	var f io.ReadSeeker
	f = filereader.NewFile(ft.blobStore, m)

	// Check if the file is requested for download (?dl=1)
	httputil.SetAttachment(m.Name, r, w)

	// Support for resizing image on the fly
	if err := resize.Resize(m.Name, f, r); err != nil {
		panic(err)
	}

	// Serve the file content using the same code as the `http.ServeFile` (it'll handle HEAD request)
	mtime, _ := m.Mtime()
	http.ServeContent(w, r, m.Name, mtime, f)
}

// Fetch a Node outside any FS
func (ft *FileTreeExt) nodeHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check permissions
		// permissions.CheckPerms(r, PermName)

		// TODO(tsileo): limit the max depth of the tree configurable via query args
		if r.Method != "GET" && r.Method != "HEAD" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		vars := mux.Vars(r)

		hash := vars["ref"]
		n, err := ft.nodeByRef(hash)
		if err != nil {
			if err == clientutil.ErrBlobNotFound {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			panic(err)
		}

		// Output some headers about ACLs
		u := &url.URL{Path: fmt.Sprintf("/%s/%s", n.Type[0:1], n.Hash)}
		pubHeader := "0"
		if n.meta.IsPublic() {
			pubHeader = "1"
			w.Header().Add("BlobStash-FileTree-Public-Path", u.String())
		}
		w.Header().Add("BlobStash-FileTree-Public", pubHeader)

		if r.URL.Query().Get("bewit") == "1" {
			if err := bewit.Bewit(ft.sharingCred, u, ft.shareTTL); err != nil {
				panic(err)
			}
			w.Header().Add("BlobStash-FileTree-SemiPrivate-Path", u.String())
			w.Header().Add("BlobStash-FileTree-Bewit", u.Query().Get("bewit"))
		}

		if r.Method == "HEAD" {
			return
		}

		if err := ft.fetchDir(n, 1, 1); err != nil {
			panic(err)
		}

		httputil.WriteJSON(w, map[string]interface{}{
			"node": n,
		})
	}
}

// nodeByRef fetch the blob containing the `meta.Meta` and convert it to a `Node`
func (ft *FileTreeExt) nodeByRef(hash string) (*Node, error) {
	blob, err := ft.blobStore.Get(context.TODO(), hash)
	if err != nil {
		return nil, err
	}

	m, err := meta.NewMetaFromBlob(hash, blob)
	if err != nil {
		return nil, err
	}

	n, err := metaToNode(m)
	if err != nil {
		return nil, err
	}

	return n, nil
}

// Dummy hanler for 404 responses
func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<!doctype html><title>BlobStash</title><p>%s</p>\n", http.StatusText(http.StatusNotFound))

}

// dirHandler serve the directory like a standard directory (an HTML page with links to each Node)
func (ft *FileTreeExt) dirHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var authorized bool

		if err := bewit.Validate(r, ft.sharingCred); err != nil {
			ft.log.Debug("invalid bewit", "err", err)
		} else {
			ft.log.Debug("valid bewit")
			authorized = true
		}

		vars := mux.Vars(r)

		hash := vars["ref"]
		n, err := ft.nodeByRef(hash)
		if err != nil {
			if err == clientutil.ErrBlobNotFound {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			panic(err)
		}

		if !authorized && n.meta.IsPublic() {
			ft.log.Debug("XAttrs public=1")
			authorized = true
		}

		if !authorized {
			// Returns a 404 to prevent leak of hashes
			ft.log.Info("Unauthorized access")
			notFound(w)
			return
		}

		if n.Type != "dir" {
			panic(httputil.NewPublicErrorFmt("node is not a dir (%s)", n.Type))
		}

		if r.Method == "HEAD" {
			return
		}

		if err := ft.fetchDir(n, 1, 1); err != nil {
			panic(err)
		}

		sort.Sort(byName(n.Children))

		// Check if the dir contains an "index.html")
		for _, cn := range n.Children {
			if cn.Name == indexFile {
				ft.serveFile(w, r, cn.Hash)
				return
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><title>BlobStash - %s</title><pre>\n", n.Name)
		for _, cn := range n.Children {
			u := &url.URL{Path: fmt.Sprintf("/%s/%s", cn.Type[0:1], cn.Hash)}

			// Only compute the Bewit if the node is not public
			if !cn.meta.IsPublic() {
				if err := bewit.Bewit(ft.sharingCred, u, ft.shareTTL); err != nil {
					panic(err)
				}
			}
			fmt.Fprintf(w, "<a href=\"%s\">%s</a>\n", u.String(), cn.Name)
		}
		fmt.Fprintf(w, "</pre>\n")
	}
}