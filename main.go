package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	htmlTemplate "html/template"
	"io"
	"log"
	"net/http"
	"strings"
	textTemplate "text/template"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
)

const port = 8080

//go:embed index.html
var indexHtml embed.FS

// ErrorLevel is the type of error found
type ErrorLevel string

const (
	misunderstoodError ErrorLevel = "misunderstood"
	parseErrorLevel    ErrorLevel = "parse"
	execErrorLevel     ErrorLevel = "exec"
)

type templateError struct {
	Line        int
	Char        int
	Description string
	Level       ErrorLevel
}
type indexData struct {
	RawText        string
	RawData        string
	RawFunctions   string
	TextLines      []string
	Output         string
	Errors         []templateError
	LineNumSpacing int
}

func getText(r *http.Request) (string, error) {
	file, _, err := r.FormFile("from-file")
	if err != nil {
		return r.FormValue("from-raw-text"), nil
	}
	defer file.Close()
	var buf bytes.Buffer
	io.Copy(&buf, file)
	return buf.String(), nil
}

func main() {
	fns := htmlTemplate.FuncMap{
		"intRange": intRange,
		"nl":       nl,
		"split":    split,
	}
	index, err := htmlTemplate.New("index.html").Funcs(fns).ParseFS(indexHtml, "*")
	if err != nil {
		panic(err)
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	a := &App{index: index}
	r.Post("/", a.Post)
	r.Get("/", a.Get)

	log.Printf("starting on port %d\n", port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), r))
}

func nl() string              { return "\n" }
func split(s string) []string { return strings.Split(s, ": ") }

func intRange(start, end int) []int {
	n := end - start + 1
	result := make([]int, n)
	for i := 0; i < n; i++ {
		result[i] = start + i
	}
	return result
}

type App struct {
	index   *htmlTemplate.Template
	tplErrs []templateError
}

var indexDataSamples = []indexData{
	{
		RawText: `<!-- https://stackoverflow.com/questions/16734503/access-out-of-loop-value-inside-golang-templates-loop -->
{{- range .Pages}}
<li><a href="{{$.Name}}/{{.}}">{{.}}</a></li>
{{- end}}`,
		RawData: `{"Name":"兵哥哥", "Pages":["立正","齐步走"]}`,
	},
}

func (a *App) Get(w http.ResponseWriter, r *http.Request) {
	a.tplErrs = make([]templateError, 0)

	for _, v := range indexDataSamples {
		data := a.createData(v.RawText, v.RawData, v.RawFunctions)
		w.Header().Add("X-XSS-Protection", "0")
		if err := a.index.Execute(w, data); err != nil {
			http.Error(w, fmt.Sprintf("Execute error: %v", err), http.StatusForbidden)
		}
		return
	}

	if err := a.index.Execute(w, indexData{}); err != nil {
		http.Error(w, fmt.Sprintf("Execute error: %v", err), http.StatusForbidden)
	}
}

const maxRequestSize = 32 << 20

func (a *App) Post(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxRequestSize); err != nil {
		http.Error(w, fmt.Sprintf("ParseMultipartForm error: %v", err), http.StatusForbidden)
		return
	}

	a.tplErrs = make([]templateError, 0)

	text, err := getText(r)
	if err == http.ErrMissingFile {
		a.tplErrs = append(a.tplErrs, templateError{Line: -1, Char: -1, Description: "couldn't accept file"})
	} else if err != nil {
		panic(err)
	}

	rawData := r.FormValue("data")
	rawFns := r.FormValue("functions")

	// outputs html into the textarea, so chrome gets worried
	// https://stackoverflow.com/a/17815577/2178159
	data := a.createData(text, rawData, rawFns)
	w.Header().Add("X-XSS-Protection", "0")
	if err := a.index.Execute(w, data); err != nil {
		http.Error(w, fmt.Sprintf("Execute error: %v", err), http.StatusForbidden)
		return
	}
}

func (a *App) createData(text, rawData, rawFns string) indexData {
	var data interface{}
	if rawData != "" {
		if err := json.Unmarshal([]byte(rawData), &data); err != nil {
			a.tplErrs = append(a.tplErrs, templateError{Line: -1, Char: -1, Level: misunderstoodError,
				Description: fmt.Sprintf("failed to understand data: %v", err)})
		}
	}

	t := textTemplate.New("input template")

	// mock template functions - this'll happen automatically as they're found, but errors will be output and there's a max limit
	var functions []string
	if rawFns != "" {
		functions = strings.Split(rawFns, ",")
	}
	for _, fn := range functions {
		fn = strings.TrimSpace(fn)
		// wrap in func so we can catch panics on bad function names
		func() {
			defer func() {
				if r := recover(); r != nil {
					a.tplErrs = append(a.tplErrs, templateError{Line: -1, Char: -1, Level: misunderstoodError,
						Description: fmt.Sprintf(`bad function name provided: "%s"`, fn)})
				}
			}()
			t = t.Funcs(textTemplate.FuncMap{fn: func() error { return nil }})
		}()
	}

	parsedT, parseTplErrs := parse(text, t)
	a.tplErrs = append(a.tplErrs, parseTplErrs...)

	var buf bytes.Buffer
	execTplErrs := exec(parsedT, data, &buf)
	a.tplErrs = append(a.tplErrs, execTplErrs...)

	lines := SplitLines(text)
	return indexData{
		RawText:        text,
		RawData:        rawData,
		RawFunctions:   rawFns,
		Output:         buf.String(),
		Errors:         a.tplErrs,
		TextLines:      lines,
		LineNumSpacing: CountDigits(len(lines)),
	}
}
