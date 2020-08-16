package jsonapi

import (
	"github.com/neuronlabs/neuron/mapping"
	"github.com/neuronlabs/neuron/server"
)

// ModelHandler is a struct that matches given Model with its API handler.
type ModelHandler struct {
	Model   mapping.Model
	Handler interface{}
}

// Options is a structure that defines json:api settings.
type Options struct {
	// PathPrefix is the path prefix used for all endpoints within given API.
	PathPrefix string
	// DefaultPageSize defines default PageSize for the list endpoints.
	DefaultPageSize int
	// NoContentOnCreate allows to set the flag for the models with client generated id to return no content.
	NoContentOnInsert bool
	// StrictFieldsMode defines if the during unmarshal process the query should strictly check
	// if all the fields are well known to given model.
	StrictUnmarshal bool
	// IncludeNestedLimit is a maximum value for nested includes (i.e. IncludeNestedLimit = 1
	// allows ?include=posts.comments but does not allow ?include=posts.comments.author)
	IncludeNestedLimit int
	// FilterValueLimit is a maximum length of the filter values
	FilterValueLimit int
	// MarshalLinks is the default behavior for marshaling the resource links into the handler responses.
	PayloadLinks bool
	// Middlewares are global middlewares added to each endpoint in the given API.
	Middlewares server.MiddlewareChain
	// DefaultHandlerModels are the models assigned to the default API handler.
	DefaultHandlerModels []mapping.Model
	// ModelHandlers are the models with their paired API handlers.
	ModelHandlers []ModelHandler
}

type Option func(o *Options)

// WithPathPrefix is an option that sets the API base path.
// The base path is a path p
func WithPathPrefix(path string) Option {
	return func(o *Options) {
		o.PathPrefix = path
	}
}

// WithDefaultPageSize is an option that sets the default page size.
func WithDefaultPageSize(pageSize int) Option {
	return func(o *Options) {
		o.DefaultPageSize = pageSize
	}
}

// WithStrictUnmarshal sets the api option for strict codec unmarshal.
func WithStrictUnmarshal() Option {
	return func(o *Options) {
		o.StrictUnmarshal = true
	}
}

// WithPayloadLinks
func WithPayloadLinks(payloadLinks bool) Option {
	return func(o *Options) {
		o.PayloadLinks = payloadLinks
	}
}

// WithMiddlewares is an option that sets global API middlewares.
func WithMiddlewares(middlewares ...server.Middleware) Option {
	return func(o *Options) {
		o.Middlewares = append(o.Middlewares, middlewares...)
	}
}

// WithNoContentOnInsert is an option that tells API to return http.StatusNoContent if an endpoint
// allows client generated primary key, and given insert is accepted.
func WithNoContentOnInsert() Option {
	return func(o *Options) {
		o.NoContentOnInsert = true
	}
}

// WithDefaultHandlerModels is an option that sets the models for the API that would use default API handler.
func WithDefaultHandlerModels(model ...mapping.Model) Option {
	return func(o *Options) {
		o.DefaultHandlerModels = append(o.DefaultHandlerModels, model...)
	}
}

// WithModelHandler is an option that sets the model handler interfaces.
func WithModelHandler(model mapping.Model, handler interface{}) Option {
	return func(o *Options) {
		o.ModelHandlers = append(o.ModelHandlers, ModelHandler{Model: model, Handler: handler})
	}
}
