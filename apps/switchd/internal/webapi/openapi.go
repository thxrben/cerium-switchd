package webapi

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/thxrben/cerium-switchd/lib/platform/version"
)

var pathParam = regexp.MustCompile(`\{([a-z]+)\}`)

// openAPI serves the description of every endpoint (OpenAPI 3.1), built
// from the route table.
func (s *Server) openAPI(w http.ResponseWriter, _ *http.Request) {
	paths := map[string]map[string]any{}
	for _, rt := range s.table() {
		op := map[string]any{"summary": rt.summary, "responses": map[string]any{
			"200": map[string]any{"description": "OK (JSON; errors are {\"error\": \"…\"} with a 4xx/5xx status)"}}}
		if rt.access != "" {
			op["security"] = []map[string][]string{{"basic": {}}, {"token": {}}}
			op["x-ceros-class"] = rt.access
		} else {
			op["security"] = []map[string][]string{}
		}
		var params []map[string]any
		for _, m := range pathParam.FindAllStringSubmatch(rt.path, -1) {
			params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
		}
		if params != nil {
			op["parameters"] = params
		}
		if paths[rt.path] == nil {
			paths[rt.path] = map[string]any{}
		}
		paths[rt.path][strings.ToLower(rt.method)] = op
	}
	reply(w, http.StatusOK, map[string]any{
		"openapi": "3.1.0",
		"info": map[string]string{"title": "cerOS REST API", "version": version.Version,
			"description": "The REST API of a cerOS switch or virtual chassis (reference 5.1, system services web-management)."},
		"components": map[string]any{"securitySchemes": map[string]any{
			"basic": map[string]string{"type": "http", "scheme": "basic"},
			"token": map[string]string{"type": "http", "scheme": "bearer", "description": "system services web-management api-token"},
		}},
		"paths": paths,
	})
}
