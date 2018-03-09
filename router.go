// Package lux contains types for creating an HTTP router for use in AWS lambda functions. The router supports
// RESTful HTTP methods & contains configuration for logging, request filtering & panic recovery.
package lux

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/aws/aws-lambda-go/events"
)

var (
	errNotAllowed    = errors.New("not allowed")
	errNotAcceptable = errors.New("not acceptable")
)

type (
	// The Router type handles incoming requests & routes them to the registered
	// handlers.
	Router struct {
		routes     []*Route
		middleware []HandlerFunc
		recovery   RecoverFunc
		log        *logrus.Logger
	}

	// The Route type defines a route that can be used by the router.
	Route struct {
		handler HandlerFunc
		method  string
		headers map[string]string
		queries map[string]string
	}

	// The ResponseWriter type allows for interacting with the HTTP response similarly to a triaditional
	// HTTP handler in go.
	ResponseWriter interface {
		Write([]byte) (int, error)
		WriteHeader(int)
		Header() *Headers
	}

	// The PanicInfo type is passed to any custom registered panic handler functions and provides details
	// on the request that caused the panic.
	PanicInfo struct {
		Error   error
		Stack   []byte
		Request Request
	}

	// The HandlerFunc type defines what a handler function should look like.
	HandlerFunc func(ResponseWriter, *Request)

	// The RecoverFunc type defines what a panic recovery function should look like.
	RecoverFunc func(PanicInfo)

	// The Request type represents an incoming HTTP request.
	Request events.APIGatewayProxyRequest

	// The Response type represents an outgoing HTTP response.
	Response events.APIGatewayProxyResponse

	// The Headers type represents the HTTP response headers.
	Headers map[string]string

	responseWriter struct {
		code    int
		headers Headers
		body    []byte
	}
)

// NewRouter creates a new lambda router.
func NewRouter() *Router {
	return &Router{
		routes:     []*Route{},
		middleware: []HandlerFunc{},
		log:        logrus.New(),
	}
}

// Handler adds a given handler to the router.
func (r *Router) Handler(method string, fn HandlerFunc) *Route {
	route := &Route{
		handler: fn,
		method:  method,
		headers: make(map[string]string),
		queries: make(map[string]string),
	}

	r.routes = append(r.routes, route)

	r.log.WithFields(logrus.Fields{
		"method": method,
	}).Info("registered new handler")

	return route
}

// Middleware adds a middleware function to the router. These methods will be called
// prior to the route handler and allow you to perform processing on the request before
// your handler is executed.
func (r *Router) Middleware(fn HandlerFunc) *Router {
	r.middleware = append(r.middleware, fn)

	return r
}

// Recovery sets a custom recovery handler that allows you to process panics using
// your own handler. Not providing a recovery handler does not mean that your
// panics are not handled. When no custom handler is specified your panic
// will be logged to os.Stdout and execution can resume.
func (r *Router) Recovery(fn RecoverFunc) *Router {
	r.recovery = fn

	return r
}

// Logging sets the output for logs generated by the router. The logging package used
// is logrus (https://github.com/sirupsen/logrus). All logs written to os.Stdout and
// os.Stderr will automatically be picked up by CloudWatch. The logrus.Formatter
// interface allows you to specify a custom format for logs. By default, the output
// is JSON
func (r *Router) Logging(out io.Writer, format logrus.Formatter) *Router {
	r.log.Formatter = format
	r.log.Out = out

	return r
}

// ServeHTTP handles an incoming HTTP request from the AWS API Gateway. If
// the request matches a registered route then the specified handler will be
// executed after any registered middleware.
//
// If a handler cannot be found matching the HTTP method, a 405 response
// will be returned to the client.
//
// If you have specified query or header filters to your route, a request
// that matches the HTTP method but lacks the required parameters/headers
// will result in a 406 response.
//
// A panic will result in a 500 response.
func (r *Router) ServeHTTP(req Request) (Response, error) {
	ts := time.Now()

	r.log.WithFields(logrus.Fields{
		"method":    req.HTTPMethod,
		"params":    req.QueryStringParameters,
		"requestId": req.RequestContext.RequestID,
	}).Info("handling incoming request")

	route, err := r.findRoute(req)

	if err == errNotAllowed {
		return newResponse(err.Error(), http.StatusMethodNotAllowed)
	}

	if err == errNotAcceptable {
		return newResponse(err.Error(), http.StatusNotAcceptable)
	}

	w := &responseWriter{
		headers: make(Headers),
		body:    []byte{},
	}

	r.performRequest(route, w, req)

	resp := w.getResponse()

	r.log.WithFields(logrus.Fields{
		"status":    resp.StatusCode,
		"duration":  time.Since(ts).String(),
		"requestId": req.RequestContext.RequestID,
	}).Info("finished handling request")

	return resp, nil
}

// performRequest executes any registered middleware before attempting to use the route's
// handler & will recover from any panics.
func (r *Router) performRequest(route *Route, w *responseWriter, req Request) {
	defer r.recover(req)

	// Run any registered middleware
	for _, mid := range r.middleware {
		// Return a response if the middleware warrants it
		if mid(w, &req); w.code != 0 {
			return
		}
	}

	route.handler(w, &req)
}

// Headers allows you to specify headers a request should have in order to
// use this route. You can use wildcards when you only care about a header's
// presence rather than its value.
func (r *Route) Headers(pairs ...string) *Route {
	r.headers = mapPairs(pairs...)

	return r
}

// Queries allows you to specify query parameters and values a request should have
// in order to use this route. You can use wildcards when you only care about a
// parameter's presence rather than its value.
func (r *Route) Queries(pairs ...string) *Route {
	r.queries = mapPairs(pairs...)

	return r
}

// newResponse creates a new response object with a JSON encoded body and given
// status code.
func newResponse(data interface{}, status int) (Response, error) {
	json, err := json.Marshal(data)

	if err != nil {
		return Response{}, fmt.Errorf("failed to encode response body, %v", err)
	}

	resp := Response{
		StatusCode: status,
		Body:       string(json),
		Headers:    make(map[string]string),
	}

	resp.Headers["Content-Type"] = "application/json"

	return resp, nil
}

// Write appends the given data to the response body.
func (w *responseWriter) Write(data []byte) (int, error) {
	w.body = append(w.body, data...)

	return len(data), nil
}

// WriteHeader writes the given HTTP status code to the HTTP response.
func (w *responseWriter) WriteHeader(code int) {
	w.code = code
}

// Headers obtains the HTTP response headers for a request.
func (w *responseWriter) Header() *Headers {
	return &w.headers
}

// Set creates a new header with the given key and value.
func (h Headers) Set(key, val string) {
	h[key] = val
}

// findRoute attempts to locate a route that can handle a given request and
// returns errors specifying if no route is found, or the provided headers &
// parameters for that route are invalid.
func (r *Router) findRoute(req Request) (*Route, error) {
	var out *Route
	var checkRoutes []*Route
	var err error

	// Look through each route
	for _, route := range r.routes {
		// If the route method matches, add it to the slice.
		if route.method == req.HTTPMethod {
			checkRoutes = append(checkRoutes, route)
		}
	}

	// If we got no routes to check, return a 405
	if len(checkRoutes) == 0 {
		return nil, errNotAllowed
	}

	// Look at each route with a matching method
	for _, route := range checkRoutes {
		err = route.canRoute(req)

		// If we cannot use this route, check the next one.
		if err != nil {
			continue
		}

		// Otherwise, we found our route
		out = route
		err = nil
		break
	}

	// If we found a route, 'out' will be non-nil.
	return out, err
}

// recover handles panics that may occur during execution of the lambda function. In a situation
// where a panic does occur, the router will recover and execute a custom panic handler if it has
// been provided.
func (r *Router) recover(req Request) {
	var err error

	// If a panic was thrown
	if rec := recover(); rec != nil {
		// Attempt to parse it
		switch x := rec.(type) {
		case string:
			err = errors.New(x)
		case error:
			err = x
		default:
			err = fmt.Errorf("%s", x)
		}

		r.log.WithFields(logrus.Fields{
			"requestId": req.RequestContext.RequestID,
			"error":     err.Error(),
		}).Error("recovered from panic")

		info := PanicInfo{
			Error:   err,
			Request: req,
			Stack:   make([]byte, 1024*8),
		}

		runtime.Stack(info.Stack, false)

		// If a custom recover func was defined, use it.
		if r.recovery != nil {
			r.recovery(info)
		}
	}
}

// canRoute determines if a route can handle a given request based on the route's expected headers
// and parameters.
func (r *Route) canRoute(req Request) error {
	if !matchMap(r.headers, req.Headers) || !matchMap(r.queries, req.QueryStringParameters) {
		return errNotAcceptable
	}

	return nil
}

// getResponse takes all data written to the response writer and converts it into a Response type
// that can be returned to the client.
func (w *responseWriter) getResponse() Response {
	if w.code == 0 {
		return Response{
			StatusCode: http.StatusInternalServerError,
			Body:       "failed to obtain response",
		}
	}

	return Response{
		StatusCode: w.code,
		Body:       string(w.body),
		Headers:    w.headers,
	}
}

// matchMap determines whether or not the keys/values from the first map
// match the keys/values in the second.
func matchMap(m1, m2 map[string]string) bool {
	// The first map contains the values we expect to be in the second
	// map.
	for expKey, expVal := range m1 {
		// If the value we expect does not exist in the map, return false.
		if value, ok := m2[expKey]; !ok || (value != expVal && expVal != "*") {
			return false
		}
	}

	return true
}

// mapPairs converts a given number of string arguments to a map. If an odd number
// of arguments are specified, the last one will be given a wildcard (*) value.
func mapPairs(pairs ...string) map[string]string {
	out := make(map[string]string)

	for i := 0; i < len(pairs); i += 2 {
		key := pairs[i]
		value := ""

		// Use a wildcard for odd pairings
		if len(pairs) < i+1 {
			value = "*"
		} else {
			value = pairs[i+1]
		}

		// Set the required value.
		out[key] = value
	}

	return out
}
