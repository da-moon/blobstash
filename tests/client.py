from hashlib import blake2b
from urllib.parse import urljoin
import base64
import os

import requests
BASE_URL = 'http://localhost:8050'
API_KEY = '123'


class KeyValue:
    def __init__(self, key, version, data=None, hash=None):
        self.key = key
        self.data = None
        if data:
            self.data = base64.b64decode(data)
        self.hash = hash
        self.version = version

    def __str__(self):
        return '<KeyValue key={!r}, version={}>'.format(self.key, self.version)

    def __repr__(self):
        return self.__str__()



class Blob:
    def __init__(self, hash, data):
        self.hash = hash
        self.data = data

    @classmethod
    def from_data(cls, data):
        h = blake2b(digest_size=32)
        h.update(data)
        return cls(h.hexdigest(), data)

    @classmethod
    def from_random(cls, size=512):
        return cls.from_data(os.urandom(size))

    def __str__(self):
        return '<Blob hash={}>'.format(self.hash)

    def __repr__(self):
        return self.__str__()


class Client:
    def __init__(self, base_url=None, api_key=None):
        self.base_url = base_url or BASE_URL
        self.api_key = api_key or API_KEY

    def _get(self, path, params={}):
        r = requests.get(urljoin(self.base_url, path), auth=('', self.api_key), params={})
        r.raise_for_status()
        return r

    def put_blob(self, blob):
        files = {blob.hash: blob.data}
        r = requests.post(urljoin(self.base_url, '/api/blobstore/upload'), auth=('', self.api_key), files=files)
        r.raise_for_status()
        return r

    def get_blob(self, hash, to_blob=True):
        r = self._get('/api/blobstore/blob/{}'.format(hash))
        if not to_blob:
            return r
        r.raise_for_status()
        return Blob(hash, r.content)

    def put_kv(self, key, data, ref='', version=-1):
        r = requests.post(
            urljoin(self.base_url, '/api/kvstore/key/'+key),
            auth=('', self.api_key),
            data=dict(data=data, ref=ref, version=version),
        )
        r.raise_for_status()
        return r.json()

    def get_kv(self, key, to_kv=False):
        data = self._get('/api/kvstore/key/'+key).json()
        if to_kv:
            return KeyValue(**data)
        return data

    def get_kv_versions(self, key):
        return self._get('/api/kvstore/key/'+key+'/_versions').json()

    def get_kv_keys(self):
        return self._get('/api/kvstore/keys').json()

    def put_doc(self, collection, doc):
        r = requests.post(
            urljoin(self.base_url, '/api/docstore/'+collection),
            auth=('', self.api_key),
            json=doc,
        )
        r.raise_for_status()
        return r.json()

    def get_docs(self, collection, limit=50, cursor=''):
        return self._get('/api/docstore/'+collection+'?cursor='+cursor+'&limit='+str(limit)).json()

    def get_doc(self, collection, id):
        return self._get('/api/docstore/'+collection+'/'+id).json()
