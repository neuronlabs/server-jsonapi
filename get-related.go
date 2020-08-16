package jsonapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/neuronlabs/neuron-extensions/codec/jsonapi"
	"github.com/neuronlabs/neuron-extensions/server/http/httputil"
	"github.com/neuronlabs/neuron-extensions/server/http/log"

	"github.com/neuronlabs/neuron/codec"
	"github.com/neuronlabs/neuron/database"
	"github.com/neuronlabs/neuron/mapping"
	"github.com/neuronlabs/neuron/query"
	"github.com/neuronlabs/neuron/server"
)

// HandleGetRelation handles json:api get related endpoint for the 'model'.
// Panics if the model is not mapped for given API controller or relationName is not found.
func (a *API) HandleGetRelated(model mapping.Model, relationName string) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		mStruct := a.Controller.MustModelStruct(model)
		relation, ok := mStruct.RelationByName(relationName)
		if !ok {
			panic(fmt.Sprintf("no relation: '%s' found for the model: '%s'", relationName, mStruct.Type().Name()))
		}
		a.handleGetRelated(mStruct, relation)(rw, req)
	}
}

func (a *API) handleGetRelated(mStruct *mapping.ModelStruct, relationField *mapping.StructField) http.HandlerFunc {
	relatedStruct := relationField.Relationship().RelatedModelStruct()
	return func(rw http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		// Check the URL 'id' value.
		id := httputil.CtxMustGetID(ctx)
		if id == "" {
			log.Debugf("[GET-RELATED][%s] Empty id params", mStruct.Collection())
			err := httputil.ErrBadRequest()
			err.Detail = "Provided empty 'id' in url"
			a.marshalErrors(rw, 0, err)
			return
		}

		model := mapping.NewModel(mStruct)
		err := model.SetPrimaryKeyStringValue(id)
		if err != nil {
			log.Debugf("[GET-RELATED][%s] Invalid URL id value: '%s': '%v'", mStruct.Collection(), id, err)
			a.marshalErrors(rw, 0, err)
			return
		}
		if model.IsPrimaryKeyZero() {
			err := httputil.ErrInvalidQueryParameter()
			err.Detail = "provided zero value 'id' parameter"
			a.marshalErrors(rw, 0, err)
			return
		}
		relatedScope := query.NewScope(relatedStruct)

		// Get jsonapi codec ans parse query parameters.
		parser, ok := jsonapi.GetCodec(a.Controller).(codec.ParameterParser)
		if !ok {
			log.Errorf("jsonapi codec doesn't implement ParameterParser")
			a.marshalErrors(rw, 500, httputil.ErrInternalError())
			return
		}

		parameters := query.MakeParameters(req.URL.Query())
		if err := parser.ParseParameters(a.Controller, relatedScope, parameters); err != nil {
			a.marshalErrors(rw, 0, err)
			return
		}
		if !relationField.IsSlice() {
			if len(relatedScope.SortingOrder) > 0 {
				log.Debugf("[GET-RELATED][%s][%s] sorting is not allowed for the GET query type", mStruct, relationField)
				err := httputil.ErrInvalidQueryParameter()
				err.Detail = "Sorting is not allowed on GET single queries."
				a.marshalErrors(rw, 400, err)
				return
			}
			if relatedScope.Pagination != nil {
				log.Debugf("[GET-RELATED][%s][%s] pagination is not allowed for the GET query type", mStruct, relationField)
				err := httputil.ErrInvalidQueryParameter()
				err.Detail = "Pagination is not allowed on GET single queries."
				a.marshalErrors(rw, 400, err)
				return
			}
			if len(relatedScope.Filters) != 0 {
				log.Debugf("[GET-RELATED][%s][%s] filtering is not allowed for the GET query type", mStruct, relationField)
				err := httputil.ErrInvalidQueryParameter()
				err.Detail = "Filtering is not allowed on GET single queries."
				a.marshalErrors(rw, 400, err)
				return
			}
		}

		// queryIncludes are the included fields from the url query.
		queryIncludes := relatedScope.IncludedRelations
		var queryFieldSet mapping.FieldSet
		var fields mapping.FieldSet
		if len(relatedScope.FieldSets) == 0 {
			fields = append(relatedScope.ModelStruct.Attributes(), relatedScope.ModelStruct.RelationFields()...)
			queryFieldSet = fields
		} else {
			fields = relatedScope.FieldSets[0]
			queryFieldSet = relatedScope.FieldSets[0]
		}
		// json:api fieldset is a combination of fields + relations.
		// The same situation is with includes.
		neuronFields, neuronIncludes := parseFieldSetAndIncludes(relatedStruct, fields, queryIncludes)
		relatedScope.FieldSets = []mapping.FieldSet{neuronFields}
		relatedScope.IncludedRelations = neuronIncludes

		// Set preset filters.
		s := query.NewScope(mStruct, model)
		if err = s.Include(relationField, neuronFields...); err != nil {
			log.Errorf("[GET-RELATED][%s][%s] including relation field failed: %v", mStruct, relationField, err)
			a.marshalErrors(rw, 500, httputil.ErrInternalError())
			return
		}

		db := a.DB
		var (
			isTransactioner bool
			result          *codec.Payload
		)
		modelHandler, hasModelHandler := a.handlers[mStruct]
		if hasModelHandler {
			if w, ok := modelHandler.(server.WithContextGetRelated); ok {
				if ctx, err = w.GetRelatedWithContext(ctx); err != nil {
					a.marshalErrors(rw, 0, err)
					return
				}
			}

			var t server.GetRelatedTransactioner
			if t, isTransactioner = modelHandler.(server.GetRelatedTransactioner); isTransactioner {
				err = database.RunInTransaction(ctx, db, t.GetRelatedWithTransaction(), func(db database.DB) error {
					result, err = a.getRelationHandleChain(ctx, db, s, relatedScope, relationField)
					return err
				})
			}
		}
		if !isTransactioner {
			result, err = a.getRelationHandleChain(ctx, db, s, relatedScope, relationField)
		}
		// execute get relation handler chain.
		if err != nil {
			a.marshalErrors(rw, 0, err)
			return
		}

		linkType := codec.RelatedLink
		// but if the config doesn't allow that - set 'codec.NoLink'
		if !a.Options.PayloadLinks {
			linkType = codec.NoLink
		}
		result.ModelStruct = relatedStruct
		result.FieldSets = []mapping.FieldSet{queryFieldSet}
		result.IncludedRelations = queryIncludes
		result.MarshalLinks = codec.LinkOptions{
			Type:          linkType,
			BaseURL:       a.Options.PathPrefix,
			RootID:        id,
			Collection:    mStruct.Collection(),
			RelationField: relationField.NeuronName(),
		}
		result.MarshalSingularFormat = !relationField.Relationship().IsToMany()

		result.PaginationLinks = &codec.PaginationLinks{}
		sb := strings.Builder{}
		sb.WriteString(a.basePath())
		sb.WriteRune('/')
		sb.WriteString(mStruct.Collection())
		sb.WriteRune('/')
		sb.WriteString(id)
		sb.WriteRune('/')
		sb.WriteString(relationField.NeuronName())
		if q := req.URL.Query(); len(q) > 0 {
			sb.WriteRune('?')
			sb.WriteString(q.Encode())
		}
		result.PaginationLinks.Self = sb.String()
		a.marshalPayload(rw, result, http.StatusOK)
	}
}

func (a *API) getRelationHandleChain(ctx context.Context, db database.DB, s, relatedScope *query.Scope, relationField *mapping.StructField) (*codec.Payload, error) {
	modelHandler, hasModelHandler := a.handlers[s.ModelStruct]
	if hasModelHandler {
		beforeHandler, ok := modelHandler.(server.BeforeGetRelationHandler)
		if ok {
			if err := beforeHandler.HandleBeforeGetRelation(ctx, db, s, relatedScope, relationField); err != nil {
				return nil, err
			}
		}
	}

	handler, ok := modelHandler.(server.GetRelationHandler)
	if !ok {
		handler = a.defaultHandler
	}
	result, err := handler.HandleGetRelation(ctx, db, s, relatedScope, relationField)
	if err != nil {
		return nil, err
	}
	if hasModelHandler {
		if afterHandler, ok := modelHandler.(server.AfterGetRelationHandler); ok {
			if err = afterHandler.HandleAfterGetRelation(ctx, db, result); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
