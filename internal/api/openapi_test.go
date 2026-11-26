package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/auth"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
)

type openAPIOp struct {
	Role       string                `json:"x-role"`
	Security   []map[string][]string `json:"security"`
	Parameters []struct {
		Ref string `json:"$ref"`
	} `json:"parameters"`
	Responses map[string]json.RawMessage `json:"responses"`
}

type openAPISchema struct {
	Description string `json:"description"`
	Properties  map[string]struct {
		Description string `json:"description"`
	} `json:"properties"`
	Required []string `json:"required"`
}

type openAPI struct {
	OpenAPI    string                          `json:"openapi"`
	Paths      map[string]map[string]openAPIOp `json:"paths"`
	Components struct {
		SecuritySchemes map[string]map[string]string `json:"securitySchemes"`
		Schemas         map[string]openAPISchema     `json:"schemas"`
		Responses       map[string]json.RawMessage   `json:"responses"`
		Parameters      map[string]json.RawMessage   `json:"parameters"`
		Headers         map[string]json.RawMessage   `json:"headers"`
	} `json:"components"`
}

func parseOpenAPI(t *testing.T) openAPI {
	t.Helper()
	var doc openAPI
	require.NoError(t, json.Unmarshal(openAPIDoc, &doc))
	return doc
}

func TestOpenAPIMatchesRoutes(t *testing.T) {
	doc := parseOpenAPI(t)
	assert.True(t, strings.HasPrefix(doc.OpenAPI, "3.1."))
	assert.Equal(t, map[string]string{"type": "http", "scheme": "basic"}, doc.Components.SecuritySchemes["basicAuth"])

	want := map[string]auth.Role{}
	for _, rt := range Routes() {
		if !strings.HasPrefix(rt.Path, pathPprof) {
			want[rt.Method+" "+rt.Path] = rt.Role
		}
	}
	got := map[string]auth.Role{}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			key := strings.ToUpper(method) + " " + path
			got[key] = auth.Role(op.Role)
			if path == "/healthz" {
				assert.Empty(t, op.Security, "open")
				assert.Empty(t, op.Role)
				continue
			}
			assert.Equal(t, []map[string][]string{{"basicAuth": {}}}, op.Security, key)
			for _, code := range []string{"401", "429"} {
				assert.Contains(t, op.Responses, code, key)
			}
			if op.Role == string(auth.Editor) {
				assert.Contains(t, op.Responses, "403", key)
			}
			// A write documents X-Requested-By.
			var params []string
			for _, p := range op.Parameters {
				params = append(params, p.Ref)
			}
			if method != "get" {
				assert.Contains(t, params, "#/components/parameters/requestedBy", key)
				assert.Contains(t, op.Responses, "400", key)
			}
		}
	}
	assert.Equal(t, want, got)

	// Every reference points at something.
	text := string(openAPIDoc)
	for rest := text; ; {
		i := strings.Index(rest, `"$ref": "#/components/`)
		if i < 0 {
			break
		}
		rest = rest[i+len(`"$ref": "#/components/`):]
		kind, name, _ := strings.Cut(rest[:strings.Index(rest, `"`)], "/")
		var ok bool
		switch kind {
		case "schemas":
			_, ok = doc.Components.Schemas[name]
		case "responses":
			_, ok = doc.Components.Responses[name]
		case "parameters":
			_, ok = doc.Components.Parameters[name]
		case "headers":
			_, ok = doc.Components.Headers[name]
		}
		assert.True(t, ok, "%s/%s", kind, name)
	}

	e := newEnv(t)
	rec := e.do(http.MethodGet, "/api/v1/openapi.json", viewer...)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, openAPIDoc, rec.Body.Bytes(), "the same bytes")
	assert.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/api/v1/openapi.json").Code)
}

// jsonFields returns the JSON names of a struct type's fields, embedded
// structs flattened as encoding/json does, and which of them are always
// written.
func jsonFields(t reflect.Type) (names, always []string) {
	seen := map[string]bool{}
	var walk func(t reflect.Type, depth int)
	walk = func(t reflect.Type, depth int) {
		var embedded []reflect.Type
		for i := range t.NumField() {
			f := t.Field(i)
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				embedded = append(embedded, f.Type)
				continue
			}
			tag := f.Tag.Get("json")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "" || name == "-" || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
			if !strings.Contains(opts, "omitempty") {
				always = append(always, name)
			}
		}
		for _, e := range embedded {
			walk(e, depth+1)
		}
	}
	walk(t, 0)
	sort.Strings(names)
	sort.Strings(always)
	return names, always
}

func TestOpenAPIMatchesSpec(t *testing.T) {
	doc := parseOpenAPI(t)
	types := map[string]reflect.Type{
		"Stream":           reflect.TypeFor[spec.Stream](),
		"Input":            reflect.TypeFor[spec.Input](),
		"Transcode":        reflect.TypeFor[spec.Transcode](),
		"Rendition":        reflect.TypeFor[spec.Rendition](),
		"Audio":            reflect.TypeFor[spec.Audio](),
		"Watermark":        reflect.TypeFor[spec.Watermark](),
		"Thumbnail":        reflect.TypeFor[spec.Thumbnail](),
		"Outputs":          reflect.TypeFor[spec.Outputs](),
		"Push":             reflect.TypeFor[spec.Push](),
		"Status":           reflect.TypeFor[statusDoc](), // stream.Status as the API writes it
		"StatusTrack":      reflect.TypeFor[stream.Track](),
		"StatusTranscoder": reflect.TypeFor[stream.Transcoder](),
		"StatusPush":       reflect.TypeFor[stream.Push](),
		"Outcome":          reflect.TypeFor[stream.Outcome](),
		"PutStreamResult":  reflect.TypeFor[putResult](),
		"ApplyResult":      reflect.TypeFor[ApplyResult](),
		"ConfigStatus":     reflect.TypeFor[ConfigStatus](),
		"Asset":            reflect.TypeFor[Asset](),
		"AssetPut":         reflect.TypeFor[AssetPut](),
		"Error":            reflect.TypeFor[errorBody](),
		"ErrorDetail":      reflect.TypeFor[errorDoc](),
	}
	for name := range doc.Components.Schemas {
		assert.Contains(t, types, name, "a schema without a Go type")
	}
	for name, typ := range types {
		sch, ok := doc.Components.Schemas[name]
		if !assert.True(t, ok, "no schema %s", name) {
			continue
		}
		assert.NotEmpty(t, sch.Description, name)
		var props []string
		for p, def := range sch.Properties {
			props = append(props, p)
			assert.NotEmpty(t, def.Description, "%s.%s has no description", name, p)
		}
		sort.Strings(props)
		fields, always := jsonFields(typ)
		assert.Equal(t, fields, props, name)
		// What the documents always hold is required, and nothing else,
		// apart from the stream document, which is also a request body: its
		// name may be left out, and outputs too.
		if !strings.HasPrefix(typ.PkgPath(), "github.com/ntt0601zcoder/open-streamer/internal/spec") {
			req := append([]string(nil), sch.Required...)
			sort.Strings(req)
			assert.Equal(t, always, req, "%s: required", name)
		}
	}
	// The status document has the manager's fields, whatever the wrapper
	// does to since.
	plain, _ := jsonFields(reflect.TypeFor[stream.Status]())
	wrapped, _ := jsonFields(reflect.TypeFor[statusDoc]())
	assert.Equal(t, plain, wrapped)
}
