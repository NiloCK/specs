package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	codeGen "github.com/filecoin-project/specs/codeGen/lib"
	util "github.com/filecoin-project/specs/codeGen/util"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

var Assert = util.Assert
var CheckErr = util.CheckErr

func replaceExt(filePath string, srcExt string, dstExt string) string {
	n := len(filePath)
	Assert(n >= len(srcExt))
	Assert(filePath[n-len(srcExt):] == srcExt)
	return filePath[:n-len(srcExt)] + dstExt
}

func usageAssert(cond bool, msg string) {
	if !cond {
		fmt.Fprintf(os.Stderr, "Usage error: %v\n\n", msg)
		fmt.Fprintf(os.Stderr, USAGE, os.Args[0])
		os.Exit(1)
	}
}

const USAGE = `SYNOPSIS
	%[1]s <command> src.id [out.go]

COMMANDS
	gen <idsrc> <goout>          parse <idsrc>, compile it, and output the generated Go code to <goout>
	fmt <idsrc>                  parse <idsrc>, and overwrite the file in-place with formatted output
	sym <idsrc> SYM1 <SYM2 ...>  parse <idsrc>, and write to stdout the contents of the given symbols
	methods-json <idsrc>         parse <idsrc>, and write to stdout a JSON listing of its method prototypes

EXAMPLES
	# compile file.id to file.gen.go
	%[1]s gen a/b/file.id a/b/file.gen.go

	# format file.id
	%[1]s fmt a/b/file.id

	# format file.id to file2.id
	%[1]s fmt a/b/file.id a/b/file2.id

	# output symbol table of file.id
	%[1]s sym a/b/file.id

	# output a JSON listing of the struct/union methods defined in file.id
	%[1]s methods-json a/b/file.id
`

func main() {
	flag.Usage = func() {
		fmt.Printf(USAGE, os.Args[0])
		os.Exit(0)
	}


	flag.Parse()
	argsOrig := flag.Args()
	Assert(len(argsOrig) > 1)
	cmd := argsOrig[0]
	args := argsOrig[1:]

	var inputFilePath, outputFilePath string
	var inputFile, outputFile *os.File
	var err error

	// first argument
	if cmd == "gen" || cmd == "fmt" || cmd == "sym" || cmd == "methods-json" {
		inputFilePath = args[0]
	}

	// second argument
	if cmd == "gen" {
		Assert(len(args) == 2)
		outputFilePath = args[1]
	} else if cmd == "fmt" {
		outputFilePath = args[0] // replace file
	} else if cmd == "methods-json" {
		usageAssert(len(args) == 1, "methods-json command requires exactly one argument")
	}

	// defer opening files until they're needed
	// so that fmt can output to the input filename,
	// and so that it can handle codeGen fmt ./...

	switch cmd {
	case "gen":
		goMod := codeGen.GenGoModFromFilePath(inputFilePath)
		outputFile, err = os.Create(outputFilePath)
		CheckErr(err)
		codeGen.WriteGoMod(goMod, outputFile)

	case "fmt":
		fmtFiles(extractIdFiles(inputFilePath))

	case "sym":
		inputFile, err = os.Open(inputFilePath)
		CheckErr(err)
		Assert(len(args) >= 2)
		mod := codeGen.ParseDSLModuleFromFile(inputFile)
		decls := mod.Decls()
		declsMap := map[string]codeGen.Decl{}
		for _, decl := range decls {
			declsMap[decl.Name()] = decl
		}
		declsPrint := []codeGen.Entry{}
		for i, sym := range args[1:] {
			if i > 0 {
				declsPrint = append(declsPrint, codeGen.EntryEmpty())
			}
			decl, ok := declsMap[sym]
			if !ok {
				panic(fmt.Sprintf("Error: symbol not found: %v\n", sym))
			}
			declsPrint = append(declsPrint, codeGen.EntryDecl(decl))
		}
		codeGen.WriteDSLBlockEntries(os.Stdout, declsPrint, codeGen.WriteDSLContextInit())

	case "methods-json":
		entriesJson := []map[string]interface{}{}
		for _, idPath := range extractIdFiles(inputFilePath) {
			idFile, err := os.Open(idPath)
			CheckErr(err)
			mod := codeGen.ParseDSLModuleFromFile(idFile)
			packageName := codeGen.ExtractPackageName(idPath)
			for _, entry := range mod.ExtractMethodPrototypesToplevel([]string{packageName}) {
				entryJson := map[string]interface{}{}
				entryJson["name"] = entry.Name
				entryJson["argTypes"] = entry.ArgTypes
				entryJson["retType"] = entry.RetType
				entriesJson = append(entriesJson, entryJson)
			}
		}
		ret, err := json.MarshalIndent(entriesJson, "", "  ")
		CheckErr(err)
		fmt.Printf("%v\n", string(ret))

	default:
		Assert(false)
	}
}

func extractIdFiles(inputPath string) []string {
	if strings.HasSuffix(inputPath, "/...") {
		return findFiles(filepath.Dir(inputPath), func(path string) bool {
			return filepath.Ext(path) == ".id"
		})
	} else if strings.HasSuffix(inputPath, ".id") {
		return []string{inputPath}
	} else {
		usageAssert(false, fmt.Sprintf("Unsupported input path spec: \"%v\"", inputPath)); panic("")
	}
}

func findFiles(inputPath string, filter func(path string) bool) []string {
	var files []string
	filepath.Walk(inputPath, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			if strings.HasPrefix(info.Name(), ".") && len(info.Name()) > 1 {
				return filepath.SkipDir // skip hidden directories
			}
			return nil // keep going into dir
		}
		if !info.Mode().IsRegular() {
			return nil // not sure what it is, skip
		}
		if filter(path) { // run through user filter
			files = append(files, path)
		}
		return nil
	})
	return files
}

func fmtFile(inpath, outpath string) error {
	inf, err := os.Open(inpath)
	if err != nil {
		return err
	}
	defer inf.Close()

	mod := codeGen.ParseDSLModuleFromFile(inf)
	outb := bytes.NewBuffer(nil)
	codeGen.WriteDSLModule(outb, mod)

	// only write if there are differences.
	// TODO: make this faster. interleaved io + cpu. goroutines maybe
	// TODO: read src once. we read src twice because ParseDSLModuleFromFile
	// 			 only takes files.
	inb, err := ioutil.ReadFile(inpath)
	if err != nil {
		return err
	}

	if !bytes.Equal(outb.Bytes(), inb) {
		err := ioutil.WriteFile(outpath, outb.Bytes(), 0777)
		if err != nil {
			return err
		}
		fmt.Println(outpath) // go fmt ./... prints which files it wrote
	} else {
		// fmt.Println(inpath, "ignored")
	}
	return nil
}

func fmtFiles(files []string) {
	for _, f := range files {
		err := fmtFile(f, f)
		CheckErr(err)
	}
}
