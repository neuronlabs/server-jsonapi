package jsonapi

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"

	"github.com/julienschmidt/httprouter"

	"github.com/neuronlabs/neuron-extensions/codec/jsonapi"
	httpServer "github.com/neuronlabs/neuron-extensions/server/http"
	"github.com/neuronlabs/neuron-extensions/server/http/httputil"
	"github.com/neuronlabs/neuron-extensions/server/http/middleware"

	"github.com/neuronlabs/neuron/auth"
	"github.com/neuronlabs/neuron/codec"
	"github.com/neuronlabs/neuron/controller"
	"github.com/neuronlabs/neuron/core"
	"github.com/neuronlabs/neuron/database"
	"github.com/neuronlabs/neuron/errors"
	"github.com/neuronlabs/neuron/log"
	"github.com/neuronlabs/neuron/mapping"
	"github.com/neuronlabs/neuron/query"
	"github.com/neuronlabs/neuron/server"
)

// Compile time check if API implements httpServer.API.
var _ httpServer.API = &API{}

// API is the neuron handler that implements https://jsonapi.org server routes for neuron models.
type API struct {
	// AuthenticatorOptions are the settings used for the API mechanics.
	Options *Options
	// Server options set from the neuron core service.
	Authorizer    auth.Verifier
	Authenticator auth.Authenticator
	DB            database.DB
	Controller    *controller.Controller
	// Endpoints are API endpoints slice created after initialization.
	Endpoints []*server.Endpoint

	handlers       map[*mapping.ModelStruct]interface{}
	models         map[*mapping.ModelStruct]struct{}
	defaultHandler *DefaultHandler
}

// New creates new jsonapi API API for the Default Controller.
func New(options ...Option) *API {
	a := &API{
		Options:        &Options{PayloadLinks: true},
		handlers:       map[*mapping.ModelStruct]interface{}{},
		models:         map[*mapping.ModelStruct]struct{}{},
		defaultHandler: &DefaultHandler{},
	}
	for _, option := range options {
		option(a.Options)
	}
	return a
}

// GetEndpoints implements server.EndpointsGetter interface.
func (a *API) GetEndpoints() []*server.Endpoint {
	return a.Endpoints
}

// InitializeAPI implements httpServer.API interface.
func (a *API) InitializeAPI(options server.Options) error {
	a.Controller = options.Controller
	a.DB = options.DB
	a.Authorizer = options.Authorizer
	a.Authenticator = options.Authenticator

	a.Options.Middlewares = append(server.MiddlewareChain{
		middleware.Controller(options.Controller),
		middleware.WithCodec(jsonapi.GetCodec(options.Controller)),
	}, a.Options.Middlewares...)

	// Check if there are any models registered for given API.
	if len(a.Options.DefaultHandlerModels) == 0 && len(a.Options.ModelHandlers) == 0 {
		return errors.WrapDetf(server.ErrServerOptions, "no models provided for the json:api")
	}

	// Check the default page size.
	if a.Options.DefaultPageSize < 0 {
		return errors.WrapDetf(server.ErrServerOptions, "provided default page size with negative value: %d", a.Options.DefaultPageSize)
	}

	// Check if the base path has absolute value - if not add the leading slash to the BasePath.
	if !path.IsAbs(a.Options.PathPrefix) {
		a.Options.PathPrefix = "/" + a.Options.PathPrefix
	}

	// Check the path prefix if it is valid url path.
	if _, err := url.Parse(a.Options.PathPrefix); err != nil {
		return errors.WrapDetf(server.ErrServerOptions, "provided invalid path prefix: %v - %v", a.Options.PathPrefix, err)
	}

	if err := a.defaultHandler.Initialize(a.Controller); err != nil {
		return err
	}
	// Initialize all model handlers that implements core.Initializer and map them to related model structures.
	for _, modelHandler := range a.Options.ModelHandlers {
		mStruct, err := a.Controller.ModelStruct(modelHandler.Model)
		if err != nil {
			return err
		}
		a.models[mStruct] = struct{}{}
		initializer, ok := modelHandler.Handler.(core.Initializer)
		if ok {
			if err := initializer.Initialize(a.Controller); err != nil {
				return err
			}
		}
		if _, ok = a.handlers[mStruct]; ok {
			return errors.WrapDetf(server.ErrServerOptions, "duplicated json:api model handler for model: '%s'", mStruct)
		}
		a.handlers[mStruct] = modelHandler.Handler
	}

	// Set default handler models.
	for _, model := range a.Options.DefaultHandlerModels {
		mStruct, err := a.Controller.ModelStruct(model)
		if err != nil {
			return err
		}
		a.models[mStruct] = struct{}{}
	}

	return nil
}

// Set implements RoutesSetter.
func (a *API) SetRoutes(router *httprouter.Router) error {
	for model := range a.models {
		// Set routes for the model
		modelHandler, _ := a.handlers[model]
		// Insert
		a.setInsertRoute(router, modelHandler, model)
		// Insert Relations
		for _, relation := range model.RelationFields() {
			a.setInsertRelationRoute(router, modelHandler, model, relation)
		}

		// deleteQuery
		a.setDeleteRoute(router, modelHandler, model)
		// deleteQuery Relations
		for _, relation := range model.RelationFields() {
			a.setDeleteRelationRoute(router, modelHandler, model, relation)
		}

		// Get
		a.setGetRoute(router, modelHandler, model)
		// Get related and get relationship routes.
		for _, relation := range model.RelationFields() {
			a.setGetRelationRoute(router, modelHandler, model, relation)
			a.setGetRelationshipRoute(router, modelHandler, model, relation)
		}
		// List
		a.setListRoute(router, modelHandler, model)

		// Patch
		a.setUpdateRoute(router, modelHandler, model)
		// Patch relations
		for _, relation := range model.RelationFields() {
			a.setUpdateRelationRoute(router, modelHandler, model, relation)
		}
	}
	return nil
}

func (a *API) setInsertRoute(router *httprouter.Router, modelHandler interface{}, model *mapping.ModelStruct) {
	endpointPath := fmt.Sprintf("/%s", model.Collection())
	if a.Options.PathPrefix != "/" {
		endpointPath = a.Options.PathPrefix + endpointPath
	}
	endpoint := &server.Endpoint{
		Path:        endpointPath,
		HTTPMethod:  "POST",
		QueryMethod: query.Insert,
		ModelStruct: model,
	}
	a.Endpoints = append(a.Endpoints, endpoint)
	insertChain := append(a.Options.Middlewares, MidContentType, httputil.MidStoreEndpoint(endpoint))
	if insertMiddlewarer, ok := modelHandler.(server.InsertMiddlewarer); ok {
		insertChain = append(insertChain, insertMiddlewarer.InsertMiddlewares()...)
	}
	log.Debugf("POST %s", endpointPath)
	router.POST(endpointPath, httputil.Wrap(insertChain.Handle(a.handleInsert(model))))
}

func (a *API) setInsertRelationRoute(router *httprouter.Router, modelHandler interface{}, model *mapping.ModelStruct, relation *mapping.StructField) {
	endpointPath := fmt.Sprintf("/%s/:id/relationships/%s", model.Collection(), relation.NeuronName())
	if a.Options.PathPrefix != "/" {
		endpointPath = a.Options.PathPrefix + endpointPath
	}
	endpoint := &server.Endpoint{
		Path:        endpointPath,
		HTTPMethod:  "POST",
		QueryMethod: query.InsertRelationship,
		ModelStruct: model,
		Relation:    relation,
	}
	a.Endpoints = append(a.Endpoints, endpoint)
	chain := append(a.Options.Middlewares, MidContentType, middleware.StoreIDFromParams("id"), httputil.MidStoreEndpoint(endpoint))
	if insertMiddlewarer, ok := modelHandler.(server.InsertRelationsMiddlewarer); ok {
		chain = append(chain, insertMiddlewarer.InsertRelationsMiddlewares()...)
	}
	log.Debugf("POST %s ", endpointPath)
	router.POST(endpointPath, httputil.Wrap(chain.Handle(a.handleInsertRelationship(model, relation))))
}

func (a *API) setDeleteRoute(router *httprouter.Router, modelHandler interface{}, model *mapping.ModelStruct) {
	endpointPath := fmt.Sprintf("/%s/:id", model.Collection())
	if a.Options.PathPrefix != "/" {
		endpointPath = a.Options.PathPrefix + endpointPath
	}
	endpoint := &server.Endpoint{
		Path:        endpointPath,
		HTTPMethod:  "DELETE",
		QueryMethod: query.Delete,
		ModelStruct: model,
	}
	a.Endpoints = append(a.Endpoints, endpoint)
	chain := append(a.Options.Middlewares, middleware.StoreIDFromParams("id"), httputil.MidStoreEndpoint(endpoint))
	if middlewarer, ok := modelHandler.(server.DeleteMiddlewarer); ok {
		chain = append(chain, middlewarer.DeleteMiddlewares()...)
	}
	log.Debugf("DELETE %s", endpointPath)
	router.DELETE(endpointPath, httputil.Wrap(chain.Handle(a.handleDelete(model))))
}

func (a *API) setDeleteRelationRoute(router *httprouter.Router, modelHandler interface{}, model *mapping.ModelStruct, relation *mapping.StructField) {
	endpointPath := fmt.Sprintf("/%s/:id/relationships/%s", model.Collection(), relation.NeuronName())
	if a.Options.PathPrefix != "/" {
		endpointPath = a.Options.PathPrefix + endpointPath
	}
	endpoint := &server.Endpoint{
		Path:        endpointPath,
		HTTPMethod:  "DELETE",
		QueryMethod: query.DeleteRelationship,
		ModelStruct: model,
		Relation:    relation,
	}
	a.Endpoints = append(a.Endpoints, endpoint)
	chain := append(a.Options.Middlewares, MidContentType, middleware.StoreIDFromParams("id"), httputil.MidStoreEndpoint(endpoint))
	if middlewarer, ok := modelHandler.(server.DeleteRelationsMiddlewarer); ok {
		chain = append(chain, middlewarer.DeleteRelationsMiddlewares()...)
	}
	log.Debugf("DELETE %s ", endpointPath)
	router.DELETE(endpointPath, httputil.Wrap(chain.Handle(a.handleDeleteRelationship(model, relation))))
}

func (a *API) setGetRoute(router *httprouter.Router, modelHandler interface{}, model *mapping.ModelStruct) {
	endpointPath := fmt.Sprintf("/%s/:id", model.Collection())
	if a.Options.PathPrefix != "/" {
		endpointPath = a.Options.PathPrefix + endpointPath
	}
	endpoint := &server.Endpoint{
		Path:        endpointPath,
		HTTPMethod:  "GET",
		QueryMethod: query.Get,
		ModelStruct: model,
	}
	a.Endpoints = append(a.Endpoints, endpoint)
	chain := append(a.Options.Middlewares, MidAccept, middleware.StoreIDFromParams("id"), httputil.MidStoreEndpoint(endpoint))
	if middlewarer, ok := modelHandler.(server.GetMiddlewarer); ok {
		chain = append(chain, middlewarer.GetMiddlewares()...)
	}
	log.Debugf("GET %s", endpointPath)
	router.GET(endpointPath, httputil.Wrap(chain.Handle(a.handleGet(model))))
}

func (a *API) setGetRelationRoute(router *httprouter.Router, modelHandler interface{}, model *mapping.ModelStruct, relation *mapping.StructField) {
	endpointPath := fmt.Sprintf("/%s/:id/%s", model.Collection(), relation.NeuronName())
	if a.Options.PathPrefix != "/" {
		endpointPath = a.Options.PathPrefix + endpointPath
	}
	endpoint := &server.Endpoint{
		Path:        endpointPath,
		HTTPMethod:  "GET",
		QueryMethod: query.GetRelated,
		ModelStruct: model,
		Relation:    relation,
	}
	a.Endpoints = append(a.Endpoints, endpoint)
	chain := append(a.Options.Middlewares, MidAccept, middleware.StoreIDFromParams("id"), httputil.MidStoreEndpoint(endpoint))
	if middlewarer, ok := modelHandler.(server.GetRelationMiddlewarer); ok {
		chain = append(chain, middlewarer.GetRelatedMiddlewares()...)
	}
	log.Debugf("GET %s ", endpointPath)
	router.GET(endpointPath, httputil.Wrap(chain.Handle(a.handleGetRelated(model, relation))))
}

func (a *API) setGetRelationshipRoute(router *httprouter.Router, modelHandler interface{}, model *mapping.ModelStruct, relation *mapping.StructField) {
	endpointPath := fmt.Sprintf("/%s/:id/relationships/%s", model.Collection(), relation.NeuronName())
	if a.Options.PathPrefix != "/" {
		endpointPath = a.Options.PathPrefix + endpointPath
	}
	endpoint := &server.Endpoint{
		Path:        endpointPath,
		HTTPMethod:  "GET",
		QueryMethod: query.GetRelationship,
		ModelStruct: model,
		Relation:    relation,
	}
	a.Endpoints = append(a.Endpoints, endpoint)
	chainRelated := append(a.Options.Middlewares, MidAccept, middleware.StoreIDFromParams("id"), httputil.MidStoreEndpoint(endpoint))
	if middlewarer, ok := modelHandler.(server.GetRelationMiddlewarer); ok {
		chainRelated = append(chainRelated, middlewarer.GetRelatedMiddlewares()...)
	}
	log.Debugf("GET %s ", endpointPath)
	router.GET(endpointPath, httputil.Wrap(chainRelated.Handle(a.handleGetRelationship(model, relation))))
}

func (a *API) setListRoute(router *httprouter.Router, modelHandler interface{}, model *mapping.ModelStruct) {
	endpointPath := fmt.Sprintf("/%s", model.Collection())
	if a.Options.PathPrefix != "/" {
		endpointPath = a.Options.PathPrefix + endpointPath
	}
	endpoint := &server.Endpoint{
		Path:        endpointPath,
		HTTPMethod:  "GET",
		QueryMethod: query.List,
		ModelStruct: model,
	}
	a.Endpoints = append(a.Endpoints, endpoint)
	chain := append(a.Options.Middlewares, MidAccept, httputil.MidStoreEndpoint(endpoint))
	if middlewarer, ok := modelHandler.(server.ListMiddlewarer); ok {
		chain = append(chain, middlewarer.ListMiddlewares()...)
	}
	log.Debugf("GET %s", endpointPath)
	router.GET(endpointPath, httputil.Wrap(chain.Handle(a.handleList(model))))
}

func (a *API) setUpdateRoute(router *httprouter.Router, modelHandler interface{}, model *mapping.ModelStruct) {
	endpointPath := fmt.Sprintf("/%s/:id", model.Collection())
	if a.Options.PathPrefix != "/" {
		endpointPath = a.Options.PathPrefix + endpointPath
	}
	endpoint := &server.Endpoint{
		Path:        endpointPath,
		HTTPMethod:  "PATCH",
		QueryMethod: query.Update,
		ModelStruct: model,
	}
	a.Endpoints = append(a.Endpoints, endpoint)
	chain := append(a.Options.Middlewares, MidContentType, middleware.StoreIDFromParams("id"), httputil.MidStoreEndpoint(endpoint))
	if middlewarer, ok := modelHandler.(server.UpdateMiddlewarer); ok {
		chain = append(chain, middlewarer.UpdateMiddlewares()...)
	}
	log.Debugf("PATCH %s", endpointPath)
	router.PATCH(endpointPath, httputil.Wrap(chain.Handle(a.handleUpdate(model))))
}

func (a *API) setUpdateRelationRoute(router *httprouter.Router, modelHandler interface{}, model *mapping.ModelStruct, relation *mapping.StructField) {
	endpointPath := fmt.Sprintf("/%s/:id/relationships/%s", model.Collection(), relation.NeuronName())
	if a.Options.PathPrefix != "/" {
		endpointPath = a.Options.PathPrefix + endpointPath
	}
	endpoint := &server.Endpoint{
		Path:        endpointPath,
		HTTPMethod:  "PATCH",
		QueryMethod: query.UpdateRelationship,
		ModelStruct: model,
		Relation:    relation,
	}
	a.Endpoints = append(a.Endpoints, endpoint)
	chain := append(a.Options.Middlewares, MidContentType, middleware.StoreIDFromParams("id"), httputil.MidStoreEndpoint(endpoint))
	if middlewarer, ok := modelHandler.(server.UpdateRelationsMiddlewarer); ok {
		chain = append(chain, middlewarer.UpdateRelationsMiddlewares()...)
	}
	log.Debugf("PATCH %s ", endpointPath)
	router.PATCH(endpointPath, httputil.Wrap(chain.Handle(a.handleUpdateRelationship(model, relation))))
}

func (a *API) basePath() string {
	if a.Options.PathPrefix == "" {
		return "/"
	}
	return a.Options.PathPrefix
}

func (a *API) baseModelPath(mStruct *mapping.ModelStruct) string {
	return path.Join("/", a.Options.PathPrefix, mStruct.Collection())
}

func (a *API) writeContentType(rw http.ResponseWriter) {
	rw.Header().Add("Content-Type", jsonapi.MimeType)
}

func (a *API) jsonapiUnmarshalOptions() *codec.UnmarshalOptions {
	return &codec.UnmarshalOptions{StrictUnmarshal: a.Options.StrictUnmarshal}
}

func (a *API) marshalErrors(rw http.ResponseWriter, status int, err error) {
	errs := httputil.MapError(err)
	a.writeContentType(rw)
	// If no status is defined - set default from the errors.
	if status == 0 {
		status = codec.MultiError(errs).Status()
	}
	// Write status to the header.
	rw.WriteHeader(status)
	// Marshal errors into response writer.
	err = jsonapi.GetCodec(a.Controller).MarshalErrors(rw, errs...)
	if err != nil {
		log.Errorf("Marshaling errors: '%v' failed: %v", err, err)
	}
}

func (a *API) marshalPayload(rw http.ResponseWriter, payload *codec.Payload, status int) {
	a.writeContentType(rw)
	buf := &bytes.Buffer{}
	payloadMarshaler := jsonapi.GetCodec(a.Controller).(codec.PayloadMarshaler)
	if err := payloadMarshaler.MarshalPayload(buf, payload); err != nil {
		rw.WriteHeader(500)
		err := jsonapi.GetCodec(a.Controller).MarshalErrors(rw, httputil.ErrInternalError())
		if err != nil {
			switch err {
			case io.ErrShortWrite, io.ErrClosedPipe:
				log.Debug2f("An error occurred while writing api errors: %v", err)
			default:
				log.Errorf("Marshaling error failed: %v", err)
			}
		}
		return
	}
	rw.WriteHeader(status)
	if _, err := rw.Write(buf.Bytes()); err != nil {
		log.Errorf("Writing to response writer failed: %v", err)
	}
}

func (a *API) createListScope(model *mapping.ModelStruct, req *http.Request) (*query.Scope, error) {
	// Create a query scope and parse url parameters.
	s := query.NewScope(model)
	// Get jsonapi codec ans parse query parameters.
	parser, ok := jsonapi.GetCodec(a.Controller).(codec.ParameterParser)
	if !ok {
		log.Errorf("jsonapi codec doesn't implement ParameterParser")
		return nil, errors.WrapDet(errors.ErrInternal, "jsonapi codec doesn't implement ParameterParser")
	}

	parameters := query.MakeParameters(req.URL.Query())
	if err := parser.ParseParameters(a.Controller, s, parameters); err != nil {
		return nil, err
	}
	return s, nil
}

func (a *API) params(req *http.Request) *server.Params {
	params := &server.Params{
		Ctx:           req.Context(),
		DB:            a.DB,
		Authorizer:    a.Authorizer,
		Authenticator: a.Authenticator,
	}
	return params
}

// parseFieldSetAndIncludes parses json:api formatted fieldSet and includes into neuron-like fieldSet and includes.
func parseFieldSetAndIncludes(mStruct *mapping.ModelStruct, fieldSet mapping.FieldSet, includes []*query.IncludedRelation) (mapping.FieldSet, []*query.IncludedRelation) {
	// In json:api primary key cannot be set as the fields - it is always obligatory.
	resultFieldset := mapping.FieldSet{mStruct.Primary()}
	resultIncludes := make([]*query.IncludedRelation, len(includes))

	// Parse sub-includes and set new values to the result includes.
	for i, subInclude := range includes {
		subFieldset, subIncludedRelations := parseFieldSetAndIncludes(subInclude.StructField.Relationship().RelatedModelStruct(), subInclude.Fieldset, subInclude.IncludedRelations)
		resultIncludes[i] = &query.IncludedRelation{
			StructField:       subInclude.StructField,
			Fieldset:          subFieldset,
			IncludedRelations: subIncludedRelations,
		}
	}

	// Parse fields
	for _, field := range fieldSet {
		switch field.Kind() {
		case mapping.KindRelationshipSingle, mapping.KindRelationshipMultiple:
			if field.Relationship().Kind() == mapping.RelBelongsTo {
				if !resultFieldset.Contains(field.Relationship().ForeignKey()) {
					resultFieldset = append(resultFieldset, field.Relationship().ForeignKey())
				}
			}
			// Join jsonapi relations with includes - neuron-like includes.
			var alreadyIncluded bool
			relatedPrimary := field.Relationship().RelatedModelStruct().Primary()
			for i, subIncluded := range includes {
				if subIncluded.StructField == field {
					if !subIncluded.Fieldset.Contains(relatedPrimary) {
						fs := includes[i].Fieldset
						fs = append(fs, relatedPrimary)
					}
					alreadyIncluded = true
					break
				}
			}
			if !alreadyIncluded {
				// Create jsonapi-relationship field query.
				resultIncludes = append(resultIncludes, &query.IncludedRelation{
					StructField: field,
					Fieldset:    mapping.FieldSet{relatedPrimary},
				})
			}
		default:
			resultFieldset = append(resultFieldset, field)
		}
	}
	return resultFieldset, resultIncludes
}
