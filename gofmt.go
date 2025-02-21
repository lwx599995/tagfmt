// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file was copied from the src/cmd/gofmt/gofmt.go
// but processFile function is modified

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
)

var (
	// main operation modes
	list                 = flag.Bool("l", false, "list files whose formatting differs from tagfmt's")
	align                = flag.Bool("a", true, "align with nearby field's tag")
	write                = flag.Bool("w", false, "write result to (source) file instead of stdout")
	tagSort              = flag.Bool("s", false, "sort struct tag by key")
	tagSortOrder         = flag.String("so", "", "sort struct tag keys order e.g json|yaml|desc")
	tagSortWeight        = flag.String("sw", "", "sort struct tag keys weight e.g json=1|yaml=2|desc=-1 the higher weight, the higher the ranking, default keys weight is 0")
	doDiff               = flag.Bool("d", false, "display diffs instead of rewriting files")
	allErrors            = flag.Bool("e", false, "report all errors (not just the first 10 on different lines)")
	fill                 = flag.String("f", "", "fill key and value for field e.g json=lower(_val)|yaml=snake(_val)")
	pattern              = flag.String("p", ".*", "field name with regular expression pattern")
	inversePattern       = flag.String("P", "", "field name with inverse regular expression pattern")
	structPattern        = flag.String("sp", ".*", "struct name with regular expression pattern")
	inverseStructPattern = flag.String("sP", "", "struct name with inverse regular expression pattern")

	// debugging
	cpuprofile = flag.String("cpuprofile", "", "write cpu profile to this file")
)

func resetFlags() {
	*list = false
	*align = true
	*write = false
	*tagSort = false
	*tagSortOrder = ""
	*doDiff = false
	*allErrors = false
	*fill = ""
	*pattern = ".*"
	*inversePattern = ""
	*structPattern = ".*"
	*inverseStructPattern = ""
	*cpuprofile = ""
}

const (
	tabWidth    = 4
	printerMode = printer.UseSpaces
)

var (
	fileSet    = token.NewFileSet() // per process FileSet
	exitCode   = 0
	parserMode parser.Mode
)

// error define
var (
	ErrUnclosedQuote   = errors.New("unclosed quote")
	ErrUnclosedBracket = errors.New("unclosed bracket")
	ErrInvalidTag      = errors.New("invalid tag")
)

func NewAstError(fs *token.FileSet, n ast.Node, err error) error {
	s := fs.Position(n.Pos())
	return fmt.Errorf("%s:%d %s", filepath.Base(s.Filename), s.Line, err)
}

func report(err error) {
	scanner.PrintError(os.Stderr, err)
	exitCode = 2
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: tagfmt [flags] [path ...]\n")
	flag.PrintDefaults()
}

func initParserMode() {
	parserMode = parser.ParseComments
	if *allErrors {
		parserMode |= parser.AllErrors
	}
}

func isGoFile(f os.FileInfo) bool {
	// ignore non-Go files
	name := f.Name()
	return !f.IsDir() && !strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".go")
}

// If in == nil, the source is the contents of the file with the given filename.
func processFile(filename string, in io.Reader, out io.Writer, stdin bool) error {
	var perm os.FileMode = 0644
	if in == nil {
		f, err := os.Open(filename)
		if err != nil {
			return err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		in = f
		perm = fi.Mode().Perm()
	}
	if *inversePattern != "" {
		err := selectInit(*inversePattern, true)
		if err != nil {
			return err
		}
	} else {
		err := selectInit(*pattern, false)
		if err != nil {
			return err
		}
	}

	if *inverseStructPattern != "" {
		err := structSelectInit(*inverseStructPattern, true)
		if err != nil {
			return err
		}
	} else {
		err := structSelectInit(*structPattern, false)
		if err != nil {
			return err
		}
	}

	src, err := ioutil.ReadAll(in)
	if err != nil {
		return err
	}

	file, err := parser.ParseFile(fileSet, filename, src, parserMode)
	if err != nil {
		return err
	}

	var executor []Executor

	executor = append(executor, &tagDoctor{
		f:  file,
		fs: fileSet,
	})

	if *fill != "" {
		filler, err := newTagFill(file, fileSet, *fill)
		if err != nil {
			return err
		}
		executor = append(executor, filler)
	}

	if *tagSort {

		weights := map[string]int{}
		for _, weightStr := range strings.Split(*tagSortWeight, "|") {
			weightStr = strings.TrimSpace(weightStr)
			if strings.TrimSpace(weightStr) == "" {
				continue
			}
			keyVals := strings.Split(weightStr, "=")
			if len(keyVals) != 2 {
				return errors.New("tagSortWeight format error please check 'sw' arg")
			}
			key := strings.TrimSpace(keyVals[0])
			val, err := strconv.Atoi(strings.TrimSpace(keyVals[1]))
			if err != nil {
				return errors.New("tagSortWeight format error please check 'sw' arg: " + err.Error())
			}
			weights[key] = val
		}
		executor = append(executor, newTagSort(file, fileSet, strings.Split(*tagSortOrder, "|"), weights))
	}
	if *align {
		executor = append(executor, newTagFmt(file, fileSet))
	}
	for _, scan := range executor {
		err := scan.Scan()
		if err != nil {
			return err
		}
	}
	for _, exe := range executor {
		err := exe.Execute()
		if err != nil {
			return err
		}
	}

	var buf bytes.Buffer
	cfg := printer.Config{Mode: printerMode, Tabwidth: tabWidth}

	err = cfg.Fprint(&buf, fileSet, file)
	if err != nil {
		return err
	}
	res := buf.Bytes()

	if !bytes.Equal(src, res) {
		// formatting has changed
		if *list {
			fmt.Fprintln(out, filename)
		}
		if *write {
			// make a temporary backup before overwriting original
			bakname, err := backupFile(filename+".", src, perm)
			if err != nil {
				return err
			}
			err = ioutil.WriteFile(filename, res, perm)
			if err != nil {
				os.Rename(bakname, filename)
				return err
			}
			err = os.Remove(bakname)
			if err != nil {
				return err
			}
		}
		if *doDiff {
			data, err := diff(src, res, filename)
			if err != nil {
				return fmt.Errorf("computing diff: %s", err)
			}
			fmt.Printf("diff -u %s %s\n", filepath.ToSlash(filename+".orig"), filepath.ToSlash(filename))
			out.Write(data)
		}
	}

	if !*list && !*write && !*doDiff {
		_, err = out.Write(res)
	}

	return err
}

func visitFile(path string, f os.FileInfo, err error) error {
	if err == nil && isGoFile(f) {
		err = processFile(path, nil, os.Stdout, false)
	}
	// Don't complain if a file was deleted in the meantime (i.e.
	// the directory changed concurrently while running gofmt).
	if err != nil && !os.IsNotExist(err) {
		report(err)
	}
	return nil
}

func walkDir(path string) {
	filepath.Walk(path, visitFile)
}

func main() {
	// call gofmtMain in a separate function
	// so that it can use defer and have them
	// run before the exit.
	gofmtMain()
	os.Exit(exitCode)
}

func gofmtMain() {
	flag.Usage = usage

	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "creating cpu profile: %s\n", err)
			exitCode = 2
			return
		}
		defer f.Close()
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	initParserMode()

	if flag.NArg() == 0 {
		if *write {
			fmt.Fprintln(os.Stderr, "error: cannot use -w with standard input")
			exitCode = 2
			return
		}
		if err := processFile("<standard input>", os.Stdin, os.Stdout, true); err != nil {
			report(err)
		}
		return
	}

	for i := 0; i < flag.NArg(); i++ {
		path := flag.Arg(i)
		switch dir, err := os.Stat(path); {
		case err != nil:
			report(err)
		case dir.IsDir():
			walkDir(path)
		default:
			if err := processFile(path, nil, os.Stdout, false); err != nil {
				report(err)
			}
		}
	}
}

func writeTempFile(dir, prefix string, data []byte) (string, error) {
	file, err := ioutil.TempFile(dir, prefix)
	if err != nil {
		return "", err
	}
	_, err = file.Write(data)
	if err1 := file.Close(); err == nil {
		err = err1
	}
	if err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

func diff(b1, b2 []byte, filename string) (data []byte, err error) {
	f1, err := writeTempFile("", "gofmt", b1)
	if err != nil {
		return
	}
	defer os.Remove(f1)

	f2, err := writeTempFile("", "gofmt", b2)
	if err != nil {
		return
	}
	defer os.Remove(f2)

	cmd := "diff"
	if runtime.GOOS == "plan9" {
		cmd = "/bin/ape/diff"
	}

	data, err = exec.Command(cmd, "-u", f1, f2).CombinedOutput()
	if len(data) > 0 {
		// diff exits with a non-zero status when the files don't match.
		// Ignore that failure as long as we get output.
		return replaceTempFilename(data, filename)
	}
	return
}

// replaceTempFilename replaces temporary filenames in diff with actual one.
//
// --- /tmp/gofmt316145376	2017-02-03 19:13:00.280468375 -0500
// +++ /tmp/gofmt617882815	2017-02-03 19:13:00.280468375 -0500
// ...
// ->
// --- path/to/file.go.orig	2017-02-03 19:13:00.280468375 -0500
// +++ path/to/file.go	2017-02-03 19:13:00.280468375 -0500
// ...
func replaceTempFilename(diff []byte, filename string) ([]byte, error) {
	bs := bytes.SplitN(diff, []byte{'\n'}, 3)
	if len(bs) < 3 {
		return nil, fmt.Errorf("got unexpected diff for %s", filename)
	}
	// Preserve timestamps.
	var t0, t1 []byte
	if i := bytes.LastIndexByte(bs[0], '\t'); i != -1 {
		t0 = bs[0][i:]
	}
	if i := bytes.LastIndexByte(bs[1], '\t'); i != -1 {
		t1 = bs[1][i:]
	}
	// Always print filepath with slash separator.
	f := filepath.ToSlash(filename)
	bs[0] = []byte(fmt.Sprintf("--- %s%s", f+".orig", t0))
	bs[1] = []byte(fmt.Sprintf("+++ %s%s", f, t1))
	return bytes.Join(bs, []byte{'\n'}), nil
}

const chmodSupported = runtime.GOOS != "windows"

// backupFile writes data to a new file named filename<number> with permissions perm,
// with <number randomly chosen such that the file name is unique. backupFile returns
// the chosen file name.
func backupFile(filename string, data []byte, perm os.FileMode) (string, error) {
	// create backup file
	f, err := ioutil.TempFile(filepath.Dir(filename), filepath.Base(filename))
	if err != nil {
		return "", err
	}
	bakname := f.Name()
	if chmodSupported {
		err = f.Chmod(perm)
		if err != nil {
			f.Close()
			os.Remove(bakname)
			return bakname, err
		}
	}

	// write data to backup file
	_, err = f.Write(data)
	if err1 := f.Close(); err == nil {
		err = err1
	}

	return bakname, err
}

// change field's tag will cause the token.Pos wrong
// so I make all token.Pos step in Scan and field's tag change in Execute
type Executor interface {
	Scan() error
	Execute() error
}

var fieldFilter func(s string) bool

func selectInit(expr string, inverse bool) error {
	var err error
	selRule, err := regexp.Compile(expr)
	if err != nil {
		return err
	}
	if inverse {
		fieldFilter = func(s string) bool {
			return !selRule.MatchString(s)
		}
	} else {
		fieldFilter = func(s string) bool {
			return selRule.MatchString(s)
		}
	}
	return nil
}

var structFieldSelect func(s string) bool

func structSelectInit(expr string, inverse bool) error {
	var err error
	selRule, err := regexp.Compile(expr)
	if err != nil {
		return err
	}
	if inverse {
		structFieldSelect = func(s string) bool {
			return !selRule.MatchString(s)
		}
	} else {
		structFieldSelect = func(s string) bool {
			return selRule.MatchString(s)
		}
	}
	return nil
}
