package jsonapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/neuronlabs/neuron/codec"
	"github.com/neuronlabs/neuron/database"
	"github.com/neuronlabs/neuron/errors"
	"github.com/neuronlabs/neuron/mapping"
	"github.com/neuronlabs/neuron/query"
	"github.com/neuronlabs/neuron/server"

	"github.com/neuronlabs/neuron-extensions/codec/jsonapi"
	"github.com/neuronlabs/neuron-extensions/server/http/httputil"
	"github.com/neuronlabs/neuron-extensions/server/http/log"
)

// HandleGet handles json:api get endpoint for the 'model'. Panics if the model is not mapped for given API controller.
func (a *API) HandleGet(model mapping.Model) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		a.handleGet(a.Controller.MustModelStruct(model))(rw, req)
	}
}

func (a *API) handleGet(mStruct *mapping.ModelStruct) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		id := httputil.CtxMustGetID(req.Context())
		if id == "" {
			log.Errorf("ID value stored in the context is empty.")
			err := errors.WrapDet(server.ErrURIParameter, "invalid 'id' url parameter").
				WithDetail("Provided empty ID in query url")
			a.marshalErrors(rw, 0, err)
			return
		}

		// Create new model and set it's primary key from the url parameter.
		model := mapping.NewModel(mStruct)
		if err := model.SetPrimaryKeyStringValue(id); err != nil {
			log.Debug2f("[GET][%s] Invalid URL id value: '%s': '%v'", mStruct.Collection(), id, err)
			err := errors.WrapDet(server.ErrURIParameter, "invalid query id parameter")
			a.marshalErrors(rw, 0, err)
			return
		}

		// Disallow zero value ID.
		if model.IsPrimaryKeyZero() {
			err := errors.WrapDet(server.ErrURIParameter, "provided zero value 'id' parameter")
			a.marshalErrors(rw, 0, err)
			return
		}

		// Create a query scope and parse url parameters.
		s := query.NewScope(mStruct, model)

		// Get jsonapi codec ans parse query parameters.
		parser, ok := jsonapi.GetCodec(a.Controller).(codec.ParameterParser)
		if !ok {
			log.Errorf("jsonapi codec doesn't implement ParameterParser")
			a.marshalErrors(rw, 500, httputil.ErrInternalError())
			return
		}

		parameters := query.MakeParameters(req.URL.Query())
		if err := parser.ParseParameters(a.Controller, s, parameters); err != nil {
			log.Debugf("[GET][%s] parsing parameters: '%s' failed: '%v'", mStruct, req.URL.RawQuery, err)
			a.marshalErrors(rw, 0, err)
			return
		}
		if len(s.SortingOrder) > 0 {
			log.Debugf("[GET][%s] sorting is not allowed for the GET query type", mStruct)
			err := httputil.ErrInvalidQueryParameter()
			err.Detail = "Sorting is not allowed on GET single queries."
			a.marshalErrors(rw, 400, err)
			return
		}
		if s.Pagination != nil {
			log.Debugf("[GET][%s] pagination is not allowed for the GET query type", mStruct)
			err := httputil.ErrInvalidQueryParameter()
			err.Detail = "Pagination is not allowed on GET single queries."
			a.marshalErrors(rw, 400, err)
			return
		}
		if len(s.Filters) != 0 {
			log.Debugf("[GET][%s] filtering is not allowed for the GET query type", mStruct)
			err := httputil.ErrInvalidQueryParameter()
			err.Detail = "Filtering is not allowed on GET single queries."
			a.marshalErrors(rw, 400, err)
			return
		}

		// queryIncludes are the included fields from the url query.
		queryIncludes := s.IncludedRelations
		var queryFieldSet mapping.FieldSet
		var fields mapping.FieldSet
		if len(s.FieldSets) == 0 {
			fields = append(s.ModelStruct.Attributes(), s.ModelStruct.RelationFields()...)
			queryFieldSet = fields
		} else {
			fields = s.FieldSets[0]
			queryFieldSet = s.FieldSets[0]
		}
		// json:api fieldset is a combination of fields + relations.
		// The same situation is with includes.
		neuronFields, neuronIncludes := parseFieldSetAndIncludes(mStruct, fields, queryIncludes)
		s.FieldSets = []mapping.FieldSet{neuronFields}
		s.IncludedRelations = neuronIncludes

		ctx := req.Context()
		db := a.DB
		var (
			result          *codec.Payload
			isTransactioner bool
			err             error
		)
		modelHandler, hasModelHandler := a.handlers[mStruct]
		if hasModelHandler {
			if w, ok := modelHandler.(server.WithContextGetter); ok {
				ctx, err = w.GetWithContext(ctx)
				if err != nil {
					a.marshalErrors(rw, 0, err)
					return
				}
			}

			var t server.GetTransactioner
			if t, isTransactioner = modelHandler.(server.GetTransactioner); isTransactioner {
				err = database.RunInTransaction(ctx, db, t.GetWithTransaction(), func(db database.DB) error {
					result, err = a.getHandleChain(ctx, db, s)
					return err
				})
			}
		}
		if !isTransactioner {
			// Handle get query.
			result, err = a.getHandleChain(ctx, db, s)
		}
		if err != nil {
			log.Debugf("[GET][%s] getting result failed: %v", mStruct, err)
			a.marshalErrors(rw, 0, err)
			return
		}

		linkType := codec.ResourceLink
		// but if the config doesn't allow that - set 'jsonapi.NoLink'
		if !a.Options.PayloadLinks {
			linkType = codec.NoLink
		}
		if result.ModelStruct == nil {
			result.ModelStruct = mStruct
		}
		result.FieldSets = []mapping.FieldSet{queryFieldSet}
		result.IncludedRelations = queryIncludes

		if result.MarshalLinks.Type == codec.NoLink {
			result.MarshalLinks = codec.LinkOptions{
				Type:       linkType,
				BaseURL:    a.Options.PathPrefix,
				RootID:     id,
				Collection: mStruct.Collection(),
			}
		}
		result.MarshalSingularFormat = true
		result.PaginationLinks = &codec.PaginationLinks{}
		sb := strings.Builder{}
		sb.WriteString(a.basePath())
		sb.WriteRune('/')
		sb.WriteString(mStruct.Collection())
		sb.WriteRune('/')
		sb.WriteString(id)
		if q := req.URL.Query(); len(q) > 0 {
			sb.WriteRune('?')
			sb.WriteString(q.Encode())
		}
		result.PaginationLinks.Self = sb.String()
		a.marshalPayload(rw, result, http.StatusOK)
	}
}

func (a *API) getHandleChain(ctx context.Context, db database.DB, q *query.Scope) (*codec.Payload, error) {
	modelHandler, hasModelHandler := a.handlers[q.ModelStruct]
	if hasModelHandler {
		beforeHandler, ok := modelHandler.(server.BeforeGetHandler)
		if ok {
			if err := beforeHandler.HandleBeforeGet(ctx, db, q); err != nil {
				return nil, err
			}
		}
	}

	getHandler, ok := modelHandler.(server.GetHandler)
	if !ok {
		getHandler = a.defaultHandler
	}
	result, err := getHandler.HandleGet(ctx, db, q)
	if err != nil {
		return nil, err
	}

	if hasModelHandler {
		afterHandler, ok := modelHandler.(server.AfterGetHandler)
		if ok {
			if err := afterHandler.HandleAfterGet(ctx, db, result); err != nil {
				return nil, err
			}
		}
	}
	return result, err
}
