package models

import (
	"testing"
	"log"
	"os"
)

func TestClientDir(t *testing.T) {
	c, err := NewClient()
 	check(err)
	tdir := NewRandomTree(t, ".", 1)
	defer os.RemoveAll(tdir) 
	meta, rw, err := c.PutDir(tdir)
	log.Printf("meta: %+v, dir rw: %+v", meta, rw)
	check(err)
	rr, err := c.GetDir(meta.Hash, meta.Name + "_restored")
	defer os.RemoveAll(meta.Name + "_restored")
	check(err)
	log.Printf("rr:%+v", rr)
	if !MatchResult(rw, rr) {
		t.Error("Directory not fully restored")
	}
}
