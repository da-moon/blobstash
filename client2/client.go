package client2

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/url"
	"sync"
)

// ErrBlobNotFound is returned from a get/stat request
// if the blob does not exist.
var ErrBlobNotFound = errors.New("blob not found")
var ErrKeyNotFound = errors.New("key doest not exist")

var defaultServerAddr = "http://localhost:8050"

// KeyValue holds a singke key value pair, along with the version (the creation timestamp)
type KeyValue struct {
	Key     string `json:"key,omitempty"`
	Value   string `json:"value"`
	Version int    `json:"version"`
}

// KeyValueVersions holds the full history for a key value pair
type KeyValueVersions struct {
	Key      string      `json:"key"`
	Versions []*KeyValue `json:"versions"`
}

type KeysResponse struct {
	Keys []string `json:"keys"`
}

type KvStore struct {
	ServerAddr string
	client     *http.Client
}

func NewKvStore(serverAddr string) *KvStore {
	if serverAddr == "" {
		serverAddr = defaultServerAddr
	}
	return &KvStore{
		ServerAddr: serverAddr,
		client:     &http.Client{},
	}
}

func (kvs *KvStore) Put(key, value string, version int) (*KeyValue, error) {
	data := url.Values{}
	data.Set("value", value)
	// TODO handle version
	body := bytes.NewBufferString(data.Encode())
	request, err := http.NewRequest("PUT", kvs.ServerAddr+"/api/v1/vkv/key/"+key, body)
	if err != nil {
		return nil, err
	}
	resp, err := kvs.client.Do(request)
	if err != nil {
		return nil, err
	}
	body.Reset()
	body.ReadFrom(resp.Body)
	resp.Body.Close()
	switch resp.StatusCode {
	case 200:
		kv := &KeyValue{}
		if err := json.Unmarshal(body.Bytes(), kv); err != nil {
			return nil, err
		}
		return kv, nil
	default:
		return nil, fmt.Errorf("failed to put key %v: %v", key, body.String())
	}
}

func (kvs *KvStore) Get(key string, version int) (*KeyValue, error) {
	request, err := http.NewRequest("GET", kvs.ServerAddr+"/api/v1/blobstore/blob/"+key, nil)
	if err != nil {
		return nil, err
	}
	resp, err := kvs.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == 200:
		kv := &KeyValue{}
		if err := json.Unmarshal(body, kv); err != nil {
			return nil, err
		}
		return kv, nil
	case resp.StatusCode == 404:
		return nil, ErrBlobNotFound
	default:
		return nil, fmt.Errorf("failed to get key %v: %v", key, string(body))
	}

}

func (kvs *KvStore) Versions(key string, start, end, limit int) (*KeyValueVersions, error) {
	// TODO handle start, end and limit
	request, err := http.NewRequest("GET", kvs.ServerAddr+"/api/v1/vkv/key/"+key+"/versions", nil)
	if err != nil {
		return nil, err
	}
	resp, err := kvs.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == 200:
		kvversions := &KeyValueVersions{}
		if err := json.Unmarshal(body, kvversions); err != nil {
			return nil, err
		}
		return kvversions, nil
	case resp.StatusCode == 404:
		return nil, ErrBlobNotFound
	default:
		return nil, fmt.Errorf("failed to get key %v: %v", key, string(body))
	}
}

func (kvs *KvStore) Keys(start, end string, limit int) ([]string, error) {
	request, err := http.NewRequest("GET", kvs.ServerAddr+"/api/v1/vkv/keys", nil)
	if err != nil {
		return nil, err
	}
	resp, err := kvs.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == 200:
		keys := &KeysResponse{}
		if err := json.Unmarshal(body, keys); err != nil {
			return nil, err
		}
		return keys.Keys, nil
	default:
		return nil, fmt.Errorf("failed to get keys: %v", string(body))
	}
}

type Blob struct {
	Hash string
	Blob string
}

type BlobStore struct {
	pipeline   bool
	wg         sync.WaitGroup
	stop       chan struct{}
	blobs      chan *Blob
	ServerAddr string
	client     *http.Client
}

func NewBlobStore(serverAddr string) *BlobStore {
	if serverAddr == "" {
		serverAddr = defaultServerAddr
	}
	return &BlobStore{
		ServerAddr: serverAddr,
		client:     &http.Client{},
		blobs:      make(chan *Blob),
		stop:       make(chan struct{}),
		pipeline:   false,
	}
}

// Get fetch the given blob.
func (bs *BlobStore) Get(hash string) ([]byte, error) {
	request, err := http.NewRequest("GET", bs.ServerAddr+"/api/v1/blobstore/blob/"+hash, nil)
	if err != nil {
		return nil, err
	}
	resp, err := bs.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == 200:
		return body, nil
	case resp.StatusCode == 404:
		return nil, ErrBlobNotFound
	default:
		return nil, fmt.Errorf("failed to get blob %v: %v", hash, string(body))
	}
}

// Stat checks wether a blob exists or not.
func (bs *BlobStore) Stat(hash string) (bool, error) {
	request, err := http.NewRequest("HEAD", bs.ServerAddr+"/api/v1/blobstore/blob/"+hash, nil)
	if err != nil {
		return false, err
	}
	resp, err := bs.client.Do(request)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == 200:
		return true, nil
	case resp.StatusCode == 404:
		return false, nil
	default:
		return false, fmt.Errorf("failed to put blob %v", hash)
	}
}

func (bs *BlobStore) Stop() {
	//close(bs.stop)
	bs.wg.Wait()
}

func (bs *BlobStore) processBlobs() {
	//bs.wg.Add(1)
	//defer bs.wg.Done()
	for blob := range bs.blobs {
		//select {
		//case blob := <-bs.blobs:
		data, err := base64.StdEncoding.DecodeString(blob.Blob)
		if err != nil {
			panic(err)
		}
		if err := bs.put(blob.Hash, data); err != nil {
			panic(err)
		}
		bs.wg.Done()
		//case <-bs.stop:
		//	return
		//}
	}
}

func (bs *BlobStore) ProcessBlobs() {
	for i := 0; i < 25; i++ {
		go bs.processBlobs()
	}
	bs.pipeline = true
}

func (bs *BlobStore) Put(hash string, blob []byte) error {
	if bs.pipeline {
		bs.wg.Add(1)
		bs.blobs <- &Blob{Hash: hash, Blob: base64.StdEncoding.EncodeToString(blob)}
		return nil
	}
	return bs.put(hash, blob)
}

// Put upload the given blob, the caller is responsible for computing the SHA-1 hash
func (bs *BlobStore) put(hash string, blob []byte) error {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(hash, hash)
	if err != nil {
		return err
	}
	if _, err := part.Write(blob); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	request, err := http.NewRequest("POST", bs.ServerAddr+"/api/v1/blobstore/upload", body)
	if err != nil {
		return err
	}
	request.Header.Add("Content-Type", writer.FormDataContentType())
	resp, err := bs.client.Do(request)
	if err != nil {
		return err
	}
	body.Reset()
	body.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("failed to put blob %v", body.String())
	}
	return nil
}