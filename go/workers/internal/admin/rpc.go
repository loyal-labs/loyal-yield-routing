package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

const maxActionBody = 16 << 20

type Call struct {
	Args []json.RawMessage
	Form *multipart.Form
}

func (c Call) Arg(i int, into any) error {
	if i >= len(c.Args) {
		return huma.Error400BadRequest(fmt.Sprintf("missing argument %d", i))
	}
	if err := json.Unmarshal(c.Args[i], into); err != nil {
		return huma.Error400BadRequest(fmt.Sprintf("argument %d: %v", i, err))
	}
	return nil
}

func (c Call) FormValue(key string) string {
	if c.Form == nil || len(c.Form.Value[key]) == 0 {
		return ""
	}
	return c.Form.Value[key][0]
}

type Redirect string

func (r Redirect) Error() string { return "redirect to " + string(r) }

type reply[V any] struct {
	Value    V      `json:"value,omitempty"`
	Redirect string `json:"redirect,omitempty"`
}

func Action[V any](m *Mux, module, fn string, handle func(ctx context.Context, d *Deps, call Call) (V, error)) {
	path := "/api/admin/rpc/" + module + "/" + fn
	schema := m.API.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[reply[V]](), true, "")
	m.API.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "rpc-" + strings.NewReplacer("/", "-", "[", "", "]", "").Replace(module) + "-" + fn,
		Method:      http.MethodPost,
		Path:        path,
		Tags:        []string{"rpc"},
		Responses: map[string]*huma.Response{"200": {
			Description: "value or redirect",
			Content:     map[string]*huma.MediaType{"application/json": {Schema: schema}},
		}},
	})
	m.Router.Post(path, func(w http.ResponseWriter, r *http.Request) {
		call, err := readCall(r)
		if err == nil {
			var value V
			value, err = handle(r.Context(), m.Deps, call)
			if err == nil {
				writeJSON(w, http.StatusOK, reply[V]{Value: value})
				return
			}
		}
		var redirect Redirect
		if errors.As(err, &redirect) {
			writeJSON(w, http.StatusOK, reply[V]{Redirect: string(redirect)})
			return
		}
		writeError(w, r, err)
	})
}

func readCall(r *http.Request) (Call, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxActionBody)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(maxActionBody); err != nil {
			return Call{}, huma.Error400BadRequest("bad multipart body")
		}
		raw := r.MultipartForm.Value["$args"]
		var args []json.RawMessage
		if len(raw) != 1 || json.Unmarshal([]byte(raw[0]), &args) != nil {
			return Call{}, huma.Error400BadRequest("bad $args")
		}
		delete(r.MultipartForm.Value, "$args")
		return Call{Args: args, Form: r.MultipartForm}, nil
	}
	var args []json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		return Call{}, huma.Error400BadRequest("body must be a JSON array of arguments")
	}
	return Call{Args: args}, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var status huma.StatusError
	if errors.As(err, &status) {
		writeJSON(w, status.GetStatus(), map[string]string{"error": status.Error()})
		return
	}
	slog.Error("loyal-admin request", "error", err, "method", r.Method, "path", r.URL.Path)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}
