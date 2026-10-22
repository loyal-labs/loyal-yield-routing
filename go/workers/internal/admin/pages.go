package admin

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
)

type PageRequest struct {
	Params map[string]string
	Query  url.Values
}

var pathParam = regexp.MustCompile(`\{(\w+)\}`)

func Page[P any](m *Mux, pattern string, load func(ctx context.Context, d *Deps, req PageRequest) (P, error)) {
	path := "/api/admin/page" + pattern
	names := []string{}
	params := []*huma.Param{}
	for _, match := range pathParam.FindAllStringSubmatch(pattern, -1) {
		names = append(names, match[1])
		params = append(params, &huma.Param{Name: match[1], In: "path", Required: true, Schema: &huma.Schema{Type: "string"}})
	}
	m.API.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "page" + strings.NewReplacer("/", "-", "{", "", "}", "").Replace(strings.TrimSuffix(pattern, "/")),
		Method:      http.MethodGet,
		Path:        path,
		Tags:        []string{"page"},
		Parameters:  params,
		Responses: map[string]*huma.Response{"200": {
			Description: "view props",
			Content: map[string]*huma.MediaType{"application/json": {
				Schema: m.API.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[P](), true, ""),
			}},
		}},
	})
	m.Router.Get(path, func(w http.ResponseWriter, r *http.Request) {
		req := PageRequest{Params: map[string]string{}, Query: r.URL.Query()}
		for _, name := range names {
			req.Params[name] = chi.URLParam(r, name)
		}
		props, err := load(r.Context(), m.Deps, req)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, props)
	})
}
