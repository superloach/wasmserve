// Copyright 2018 Hajime Hoshi
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

const indexHTML = `<!DOCTYPE html>
<script src="wasm_exec.js"></script>
<script>
(async () => {
  const resp = await fetch('main.wasm');
  if (!resp.ok) {
    const pre = document.createElement('pre');
    pre.innerText = await resp.text();
    document.body.appendChild(pre);
  } else {
    const src = await resp.arrayBuffer();
    const go = new Go();
    const result = await WebAssembly.instantiate(src, go.importObject);
    go.argv = {{.Argv}};
    go.env = {{.Env}};
    go.run(result.instance);
  }
  const reload = await fetch('_wait');
  // The server sends a response for '_wait' when a request is sent to '_notify'.
  if (reload.ok) {
    location.reload();
  }
})();
</script>
`

var (
	flagHTTP        = flag.String("http", ":8080", "HTTP bind address to serve")
	flagTags        = flag.String("tags", "", "Build tags")
	flagAllowOrigin = flag.String("allow-origin", "", "Allow specified origin (or * for all origins) to make requests to this server")
	flagOverlay     = flag.String("overlay", "", "Overwrite source files with a JSON file (see https://pkg.go.dev/cmd/go for more details)")
)

var (
	tmpOutputDir = ""
	waitChannel  = make(chan struct{})
)

func ensureTmpOutputDir() (string, error) {
	if tmpOutputDir != "" {
		return tmpOutputDir, nil
	}

	tmp, err := ioutil.TempDir("", "")
	if err != nil {
		return "", err
	}
	tmpOutputDir = tmp
	return tmpOutputDir, nil
}

func handle(w http.ResponseWriter, r *http.Request) {
	if *flagAllowOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", *flagAllowOrigin)
	}

	output, err := ensureTmpOutputDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	upath := r.URL.Path[1:]
	fpath := path.Base(upath)

	if !strings.HasSuffix(r.URL.Path, "/") {
		fi, err := os.Stat(fpath)
		if err != nil && !os.IsNotExist(err) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if fi != nil && fi.IsDir() {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusSeeOther)
			return
		}
	}

	switch filepath.Base(fpath) {
	case ".":
		fpath = filepath.Join(fpath, "index.html")
		fallthrough
	case "index.html":
		if _, err := os.Stat(fpath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if errors.Is(err, fs.ErrNotExist) {
			fargs := flag.Args()
			if len(fargs) == 0 {
				fargs = []string{filepath.Join(output, "main.wasm")}
			}
			argv := make([]string, 0, len(fargs))
			for _, a := range fargs {
				argv = append(argv, `"`+template.JSEscapeString(a)+`"`)
			}
			h := strings.ReplaceAll(indexHTML, "{{.Argv}}", "["+strings.Join(argv, ", ")+"]")

			oenv := os.Environ()
			env := make([]string, 0, len(oenv))
			for _, e := range oenv {
				split := strings.SplitN(e, "=", 2)
				env = append(env, split[0]+`: "`+template.JSEscapeString(split[1])+`"`)
			}
			h = strings.ReplaceAll(h, "{{.Env}}", "{"+strings.Join(env, ", ")+"}")

			http.ServeContent(w, r, "index.html", time.Now(), bytes.NewReader([]byte(h)))
			return
		}
	case "wasm_exec.js":
		if _, err := os.Stat(fpath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if errors.Is(err, fs.ErrNotExist) {
			out, err := exec.Command("go", "env", "GOROOT").Output()
			if err != nil {
				log.Print(err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			f := filepath.Join(strings.TrimSpace(string(out)), "misc", "wasm", "wasm_exec.js")
			http.ServeFile(w, r, f)
			return
		}
	case "main.wasm":
		if _, err := os.Stat(fpath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if errors.Is(err, fs.ErrNotExist) {
			// `go run -exec cp <pkg> <output>` is used instead of the equivalent `go build -o <output> <pkg>`
			// This is to support path@version syntax.
			// A combination of GOBIN and `go install` would not work due to:
			// go: cannot install cross-compiled binaries when GOBIN is set
			exc, err := os.Executable()
			if err != nil {
				log.Print(err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			args := []string{"run", "-exec", exc}
			if *flagTags != "" {
				args = append(args, "-tags", *flagTags)
			}
			if *flagOverlay != "" {
				args = append(args, "-overlay", *flagOverlay)
			}
			if flag.NArg() > 0 {
				args = append(args, flag.Args()[0])
			} else {
				args = append(args, ".")
			}
			args = append(args, filepath.Join(output, "main.wasm"))
			log.Print("go ", strings.Join(args, " "))
			cmdRun := exec.Command("go", args...)
			cmdRun.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "WASMSERVE=cp")
			cmdRun.Dir = "."
			out, err := cmdRun.CombinedOutput()
			if err != nil {
				log.Print(err)
				log.Print(string(out))
				http.Error(w, string(out), http.StatusInternalServerError)
				return
			}
			if len(out) > 0 {
				log.Print(string(out))
			}

			f, err := os.Open(filepath.Join(output, "main.wasm"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer f.Close()

			http.ServeContent(w, r, "main.wasm", time.Now(), f)
			return
		}
	case "_wait":
		waitForUpdate(w, r)
		return
	case "_notify":
		notifyWaiters(w, r)
		return
	}

	http.ServeFile(w, r, filepath.Join(".", r.URL.Path))
}

func waitForUpdate(w http.ResponseWriter, r *http.Request) {
	waitChannel <- struct{}{}
	http.ServeContent(w, r, "", time.Now(), bytes.NewReader(nil))
}

func notifyWaiters(w http.ResponseWriter, r *http.Request) {
	for {
		select {
		case <-waitChannel:
		default:
			http.ServeContent(w, r, "", time.Now(), bytes.NewReader(nil))
			return
		}
	}
}

func main() {
	flag.Parse()
	if os.Getenv("WASMSERVE") == "cp" {
		in := flag.Args()[0]
		out := flag.Args()[1]

		inf, err := os.Open(in)
		if err != nil {
			log.Fatalln("open input", err)
		}
		defer inf.Close()

		outf, err := os.Create(out)
		if err != nil {
			log.Fatalln("create output", err)
		}
		defer outf.Close()

		if _, err := io.Copy(outf, inf); err != nil {
			log.Fatalln("copy input to output", err)
		}

		return
	}

	http.HandleFunc("/", handle)
	log.Fatal(http.ListenAndServe(*flagHTTP, nil))
}
