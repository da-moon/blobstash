BlobStash
=========

<p align="center">
  <img 
    src="https://sos-ch-dk-2.exo.io/hexaninja/blobstash.png" 
    width="192" height="192" border="0" alt="microblog.pub">
</p>

[![builds.sr.ht status](https://builds.sr.ht/~tsileo/blobstash.svg)](https://builds.sr.ht/~tsileo/blobstash?)
&nbsp; &nbsp;[![License](http://img.shields.io/badge/license-MIT-red.svg?style=flat)](https://git.sr.ht/~tsileo/blobstash/tree/master/LICENSE)

Your personal database.

**Still in early development.**

## Manifesto

BlobStash is primarily a database, you can store raw blobs, key-value pairs, JSON documents and files/directories. 

It can also acts as a web server/reverse proxy.

The web server supports HTTP/2 and can generate you TLS certs on the fly using Let's Encrypt.
You can proxy other applications and gives them free certs at the same time, you can also write apps (using Lua) that lets
you interact with BlobStash's database.
Hosting static content is also an option.
It let you easily add authentication to any app/proxied service.

### Blobs

The content-addressed blob store (the identifier of a blob is its own hash, the chosen hash function is [BLAKE2b](https://blake2.net/)) is at the heart of everything in BlobStash. Everything permanently stored in BlobStash ends up in a blob.

BlobStash has its own storage engine: [BlobsFile](https://github.com/tsileo/blobsfile), data is stored in an append-only flat file.
All data is immutable, stored with error correcting code for bit-rot protection, and indexed in a temporary index for fast access, only 2 seeks operations are needed to access any blobs.

The blob store supports real-time replication via an Oplog (powered by Server-Sent Events) to replicate to another BlobStash instance (or any system), and also support efficient synchronisation between instances using a Merkle tree to speed-up operations.

### Key-values

Key-value pairs lets you keep a mutable reference to an internal or external object, it can be a hash and/or any sequence of bytes.

Each key-value has a timestamp associated, its version. you can easily list all the versions, by default, the latest version is returned.
Internally, each "version" is stored as a separate blob, with a specific format, so it can be detected and re-indexed.

Key-Values are indexed in a temporary database (that can be rebuilt at any time by scanning all the blobs) and stored as a blob.

### Files, tree of files

Files and tree of files are first-class citizen in BlobStash.

Files are split in multiple chunks (stored as blobs, using content-defined chunking, giving deduplication at the file level), and everything is stored in a kind of Merkle tree where the hash of the JSON file containing the file metadata is the final identifier (which will also be stored as blob).

The JSON format also allow to model directory. A regular HTTP multipart endpoint can convert file to BlobStash internal format for you, or you can do it locally to prevent sending blobs that are already present.

Files can be streamed easily, range requests are supported, EXIF metadata automatically extracted and served, and on-the-fly resizing (with caching) for images.

You can also enable a S3 compatible gateway to manage your files.

### Role Based Access Control (RBAC)

BlobStash features fine-grained permissions support, with a model similar to AWS roles.

#### Predefined roles

 - `admin`: full access to everything
   - `action:*`/`resource:*`

## Document Store

The _Document Store_ stores JSON documents, think MongoDB or CouchDB, and exposes it over an HTTP API.

Documents are stored in a collection. All collections are stored in a single namespace.

Every document versions is kept (and always accessible via temporal queries, i.e. querying the state of a collection at an instant `t`).

The _Document Store_ supports ETag, conditional requests (`If-Match`...) and [JSON Patch](http://jsonpatch.com/) for partial/consistent update.

Documents are queried with Lua functions, like:

```Lua
local docstore = require('docstore')
return function(doc)
  if doc.subdoc.counter > 10 and docstore.text_search(doc, "query", {"content"}) then
    return true
  end
  return false
end
```

It also implements a basic MapReduce framework (Lua powered too).

And lastly, a document can hold pointers to filse/nodes stored in the _FileTree Store_.

Internally, a JSON document "version" is stored as a "versioned key-value" entry.
Document IDs encode the creation version, and are lexicographically sorted by creation date (8 bytes nano timestamp + 4 random bytes).
The _Versioned Key-Value Store_ is the default index for listing/sorting documents.

### Collections

#### GET /api/docstore

List all the collections.

##### HTTP Request

```shell
$ http --auth :apikey GET https://instance.com/api/docstore
```

##### HTTP Response

```json
{
    "data": [
        "mycollection"
    ], 
    "pagination": {
        "count": 1, 
        "cursor": "", 
        "has_more": false, 
        "per_page": 50
    }
}
```

##### blobstash-python

```python
from blobstash.docstore import DocStoreClient

client = DocStoreClient("https://instance.com", api_key="apikey")

client.collections()
# [blobstash.docstore.Collection(name='mycollection')]
```

### Inserting documents

Collections are created on-the-fly when a document is inserted.

#### POST /api/docstore/{collection}

##### HTTP Request

```shell
$ http --auth :apikey post https://instance.com/api/docstore/{collection} content=lol
```

##### HTTP Response

```
{
    "_created": "2020-02-23T15:28:06Z", 
    "_id": "15f6119d6dddd68fa986d4c7", 
    "_version": "1582471686918100623"
}
```

##### blobstash-python

```python
from blobstash.docstore import DocStoreClient

client = DocStoreClient("https://instance.com", api_key="apikey")

# or `client["mycol"]` or `client.collection("mycol")`
col = client.mycol

doc = {"content": "lol"}

col.insert(doc)
# blobstash.docstore.ID(_id='15f611f032ae804d668dd855')

# the `dict` will be updated with its `_id`
doc
# {'content': 'lol',
#  '_id': blobstash.docstore.ID(_id='15f611f032ae804d668dd855')}
```

### Updating a document (by replacing it)

#### POST /api/docstore/{collection}/{id}

##### HTTP Request

```shell
$ http --auth :apikey post https://instance.com/api/docstore/{collection} content=lol
```

##### HTTP Response

```
{
    "_created": "2020-02-23T15:28:06Z", 
    "_id": "15f6119d6dddd68fa986d4c7", 
    "_version": "1582471686918100623"
}
```

#### PATCH /api/docstore/{collection}/{id}

##### HTTP Request

##### HTTP Response

##### blobstash-python

### Deleting documents

#### DELETE /api/docstore/{collection}/{id}

##### HTTP Request

```shell
$ http --auth :apikey delete https://instance.com/api/docstore/{collection}/{id}
```

##### HTTP Response

204 no content.

##### blobstash-python

```python
from blobstash.docstore import DocStoreClient

client = DocStoreClient("https://instance.com", api_key="apikey")

# or `client["mycol"]` or `client.collection("mycol")`
col = client.mycol

# Can take an ID as `str`, an `ID` object, or a document (with the `_id` key)
col.delete("15f611f032ae804d668dd855")
```

### Retrieving documents

### Querying documents

#### GET /api/docstore/{collection}{?sort_index,as_of}

##### HTTP Request

```shell
$ http --auth :apikey get https://instance.com/api/docstore/{collection}
```

##### HTTP Response

```json
{
    "data": [
        {
            "_created": "2020-02-23T15:50:24Z", 
            "_id": "15f612d4f7715bdb28c93fd9", 
            "_updated": "2020-02-23T15:55:15Z", 
            "_version": "1582473315736447008", 
            "content": "lol2"
        }
    ], 
    "pagination": {
        "count": 1, 
        "cursor": "ZG9jc3RvcmU6Y29sMToxNWY2MTJkNGY3NzE1YmRiMjhjOTNmZDg=", 
        "has_more": false, 
        "per_page": 50
    }, 
    "pointers": {}
}
```

##### blobstash-python

```python
from blobstash.docstore import DocStoreClient

client = DocStoreClient("https://instance.com", api_key="apikey")

# or `client["mycol"]` or `client.collection("mycol")`
col = client.mycol

col.query()
#
```

### Sorting/indexes

Sorting can only be done through indexes.

### MapReduce framework

## BlobStash Use Cases

### Backups from external servers

Setup an API key with limited permissions (in blobstash.yaml), just enough to save a snapshot of a tree:

```yaml
# [...]
auth:
 - id: 'my_backup_key'
   password: 'my_api_key'
   roles: 'backup_server1'
roles:
 - name: 'backup_server1'
   perms:
    - action: 'action:stat:blob'
      resource: 'resource:blobstore:blob:*'
    - action: 'action:write:blob'
      resource: 'resource:blobstore:blob:*'
    - action: 'action:snapshot:fs'
      resource: 'resource:filetree:fs:server1'
    - action: 'action:write:kv'
      resource: 'resource:kvstore:kv:_filetree:fs:server1'
    - action: 'action:gc:namespace'
      resource: 'resource:stash:namespace:server1'
```

Then on "server1":

```bash
$ export BLOBS_API_HOST=https://my-blobstash-instance.com BLOBS_API_KEY=my_api_key
$ blobstash-uploader server1 /path/to/data
```

### Lua API

#### Extra module

- [`extra.glob(pattern, name)`](#extraglobpattern-name)

##### extra.glob(pattern, name)

Parses the shell file name pattern/glob and reports wether the file name matches.

Uses go's [filepath.Match](https://godoc.org/path/filepath#Match).

**Attributes**

| Name    | Type   | Description |
| ------- | ------ | ----------- |
| pattern | String | Glob pattern |
| name    | String | file name |

**Returns**

Boolean

## Contribution

Pull requests are welcome but open an issue to start a discussion before starting something consequent.

Feel free to open an issue if you have any ideas/suggestions!

## License

Copyright (c) 2014-2018 Thomas Sileo and contributors. Released under the MIT license.
