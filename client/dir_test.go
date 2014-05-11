package client

import (
	"testing"
	"os"
)

func TestClientDir(t *testing.T) {
	c, err := NewClient()
 	check(err)
	tdir := NewRandomTree(t, ".", 1)
	defer os.RemoveAll(tdir) 
	meta, _, err := c.PutDir(tdir)
	check(err)
	_, err = c.GetDir(meta.Hash, meta.Name + "_restored")
	defer os.RemoveAll(meta.Name + "_restored")
	check(err)
	//if !MatchResult(rw, rr) {
	//	t.Error("Directory not fully restored")
	//}
}

func TestClientDirDeepRecursion(t *testing.T) {
	if testing.Short() {
        t.Skip("Skipping TestClientDirDeepRecursion test in short mode.")
    }
	c, err := NewClient()
 	check(err)
	tdir := NewRandomTree(t, ".", 3)
	defer os.RemoveAll(tdir) 
	meta, _, err := c.PutDir(tdir)
	check(err)
	_, err = c.GetDir(meta.Hash, meta.Name + "_restored")
	defer os.RemoveAll(meta.Name + "_restored")
	check(err)
	//if !MatchResult(rw, rr) {
	//	t.Error("Directory not fully restored")
	//}
}