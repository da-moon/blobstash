package main

import (
	"io/ioutil"
	"path/filepath"
	"strings"

	. "github.com/dave/jennifer/jen"
)

func main() {
	files, err := filepath.Glob("lua/*.lua")
	if err != nil {
		panic(err)
	}
	f := NewFile("luascripts")
	f.Comment("Autogenerated ; DO NOT EDIT")
	f.Line()
	f.Var().Id("files").Op("=").Map(String()).String().Values(DictFunc(func(d Dict) {
		for _, fi := range files {
			dat, err := ioutil.ReadFile(fi)
			if err != nil {
				panic(err)
			}
			d[Lit(strings.Replace(fi, "lua/", "", 1))] = Lit(string(dat))
		}
	}))
	f.Save("pkg/luascripts/files.go")
}