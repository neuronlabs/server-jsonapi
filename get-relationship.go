package jsonapi

import (
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

// HandleGetRelationship handles json:api get relationship endpoint for the 'model'.
// Panics if the model is not mapped for given API controller or the relation doesn't exists.
func (a *API) HandleGetRelationship(model mapping.Model, relationName string) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		mStruct := a.Controller.MustModelStruct(model)
		relation, ok := mStruct.RelationByName(relationName)
		if !ok {
			panic(fmt.Sprintf("no relation: '%s' found for the model: '%s'", relationName, mStruct.Type().Name()))
		}
		a.handleGetRelationship(mStruct, relation)(rw, req)
	}
}

func (a *API) handleGetRelationship(mStruct *mapping.ModelStruct, relation *mapping.StructField) http.HandlerFunc {
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

		var (
			relatedScope  *query.Scope
			queryIncludes []*query.IncludedRelation
		)
		relatedModelStruct := relation.Relationship().RelatedModelStruct()
		if len(req.URL.Query()) > 0 {
			// Get jsonapi codec ans parse query parameters.
			parser, ok := jsonapi.GetCodec(a.Controller).(codec.ParameterParser)
			if !ok {
				log.Errorf("jsonapi codec doesn't implement ParameterParser")
				a.marshalErrors(rw, 500, httputil.ErrInternalError())
				return
			}
			relatedScope = query.NewScope(relatedModelStruct)

			parameters := query.MakeParameters(req.URL.Query())
			if err := parser.ParseParameters(a.Controller, relatedScope, parameters); err != nil {
				a.marshalErrors(rw, 0, err)
				return
			}
			if !relation.IsSlice() {
				if len(relatedScope.SortingOrder) > 0 {
					log.Debugf("[GET-RELATIONSHIP][%s][%s] sorting is not allowed for the GET query type", mStruct, relation)
					err := httputil.ErrInvalidQueryParameter()
					err.Detail = "Sorting is not allowed on GET single queries."
					a.marshalErrors(rw, 400, err)
					return
				}
				if relatedScope.Pagination != nil {
					log.Debugf("[GET-RELATIONSHIP][%s][%s] pagination is not allowed for the GET query type", mStruct, relation)
					err := httputil.ErrInvalidQueryParameter()
					err.Detail = "Pagination is not allowed on GET single queries."
					a.marshalErrors(rw, 400, err)
					return
				}
				if len(relatedScope.Filters) != 0 {
					log.Debugf("[GET-RELATIONSHIP][%s][%s] filtering is not allowed for the GET query type", mStruct, relation)
					err := httputil.ErrInvalidQueryParameter()
					err.Detail = "Filtering is not allowed on GET single queries."
					a.marshalErrors(rw, 400, err)
					return
				}
			}
			if len(relatedScope.FieldSets) > 0 {
				log.Debugf("[GET-RELATIONSHIP][%s][%s] field set is not allowed for the GET query type", mStruct, relation)
				err := httputil.ErrInvalidQueryParameter()
				err.Detail = "Relationship endpoint fieldset is not allowed on GET single queries."
				a.marshalErrors(rw, 400, err)
				return
			}

			// queryIncludes are the included fields from the url query.
			queryIncludes = relatedScope.IncludedRelations
			var fields mapping.FieldSet
			for _, include := range relatedScope.IncludedRelations {
				fields = append(fields, include.StructField)
			}
			// json:api fieldset is a combination of fields + relations.
			// The same situation is with includes.
			neuronFields, neuronIncludes := parseFieldSetAndIncludes(relatedModelStruct, fields, queryIncludes)
			relatedScope.FieldSets = []mapping.FieldSet{neuronFields}
			relatedScope.IncludedRelations = neuronIncludes

			for _, include := range queryIncludes {
				include.Fieldset = mapping.FieldSet{}
			}
		}

		// Set preset filters.
		s := query.NewScope(mStruct, model)
		// Get only primary key.
		s.FieldSets = []mapping.FieldSet{{mStruct.Primary()}}

		// Include relation.
		if err = s.Include(relation, relatedModelStruct.Primary()); err != nil {
			log.Errorf("[GET-RELATIONSHIP][%s][%s] Setting related field into fieldset failed: %v", mStruct.Collection(), relation.NeuronName(), err)
			a.marshalErrors(rw, 0, httputil.ErrInternalError())
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
					result, err = a.getRelationHandleChain(ctx, db, s, relatedScope, relation)
					return err
				})
			}
		}
		if !isTransactioner {
			result, err = a.getRelationHandleChain(ctx, db, s, relatedScope, relation)
		}
		// execute get relation handler chain.
		if err != nil {
			a.marshalErrors(rw, 0, err)
			return
		}

		result.ModelStruct = relatedModelStruct
		result.IncludedRelations = queryIncludes
		result.FieldSets = []mapping.FieldSet{{relatedModelStruct.Primary()}}
		linkType := codec.RelationshipLink
		// but if the config doesn't allow that - set 'codec.NoLink'
		if !a.Options.PayloadLinks {
			linkType = codec.NoLink
		}
		result.MarshalLinks = codec.LinkOptions{
			Type:          linkType,
			BaseURL:       a.Options.PathPrefix,
			RootID:        id,
			Collection:    mStruct.Collection(),
			RelationField: relation.NeuronName(),
		}
		result.MarshalSingularFormat = !relation.Relationship().IsToMany()
		result.PaginationLinks = &codec.PaginationLinks{}
		sb := strings.Builder{}
		sb.WriteString(a.basePath())
		sb.WriteRune('/')
		sb.WriteString(mStruct.Collection())
		sb.WriteRune('/')
		sb.WriteString(id)
		sb.WriteString("/relationships/")
		sb.WriteString(relation.NeuronName())
		if q := req.URL.Query(); len(q) > 0 {
			sb.WriteRune('?')
			sb.WriteString(q.Encode())
		}
		result.PaginationLinks.Self = sb.String()
		a.marshalPayload(rw, result, http.StatusOK)
	}
}
