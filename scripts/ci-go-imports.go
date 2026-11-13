// Read imports from committed Go sources without downloading modules or compiling services.
package main

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

func imports(ref string) (graph map[string][]string, resultErr error) {
	cmd := exec.Command("git", "archive", ref, "go/workers")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		pipe.Close()
		if err := cmd.Wait(); resultErr == nil {
			resultErr = err
		}
	}()
	graph = map[string][]string{}
	reader := tar.NewReader(pipe)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(header.Name, ".go") {
			continue
		}
		directory := path.Dir(header.Name)
		if graph[directory] == nil {
			graph[directory] = []string{}
		}
		if strings.HasSuffix(header.Name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), header.Name, reader, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, entry := range file.Imports {
			name, err := strconv.Unquote(entry.Path.Value)
			if err != nil {
				return nil, err
			}
			graph[directory] = append(graph[directory], name)
		}
	}
	return graph, nil
}

func main() {
	graphs := map[string]map[string][]string{}
	for _, ref := range os.Args[1:] {
		graph, err := imports(ref)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		graphs[ref] = graph
	}
	if err := json.NewEncoder(os.Stdout).Encode(graphs); err != nil {
		panic(err)
	}
}
