package jsonapi

import (
	"fmt"
	"net/http"

	"github.com/neuronlabs/neuron-extensions/codec/jsonapi"
	"github.com/neuronlabs/neuron-extensions/server/http/httputil"
	"github.com/neuronlabs/neuron-extensions/server/http/log"
	"github.com/neuronlabs/neuron/codec"
	"github.com/neuronlabs/neuron/database"
	"github.com/neuronlabs/neuron/mapping"
	"github.com/neuronlabs/neuron/query"
	"github.com/neuronlabs/neuron/server"
)

// HandleUpdateRelationship handles json:api update relationship endpoint for the 'model'.
// Panics if the model is not mapped for given API controller or the relation doesn't exists.
func (a *API) HandleUpdateRelationship(model mapping.Model, relationName string) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		mStruct := a.Controller.MustModelStruct(model)
		relation, ok := mStruct.RelationByName(relationName)
		if !ok {
			panic(fmt.Sprintf("no relation: '%s' found for the model: '%s'", relationName, mStruct.Type().Name()))
		}
		a.handleUpdateRelationship(mStruct, relation)(rw, req)
	}
}

func (a *API) handleUpdateRelationship(mStruct *mapping.ModelStruct, relation *mapping.StructField) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		// Get the id from the url.
		id := httputil.CtxMustGetID(req.Context())
		if id == "" {
			log.Debugf("[UPDATE-RELATIONSHIP][%s] Empty id params", mStruct.Collection())
			err := httputil.ErrBadRequest()
			err.Detail = "Provided empty 'id' in url"
			a.marshalErrors(rw, 0, err)
			return
		}

		model := mapping.NewModel(mStruct)
		if err := model.SetPrimaryKeyStringValue(id); err != nil {
			err := httputil.ErrInvalidQueryParameter()
			err.Detail = "provided invalid 'id' value"
			a.marshalErrors(rw, 0, err)
			return
		}

		// Check if url parameter 'id' has zero value.
		if model.IsPrimaryKeyZero() {
			err := httputil.ErrInvalidQueryParameter()
			err.Detail = "provided zero value primary key"
			a.marshalErrors(rw, 0, err)
			return
		}

		// Unmarshal relationship input.
		pu := jsonapi.GetCodec(a.Controller).(codec.PayloadUnmarshaler)
		payload, err := pu.UnmarshalPayload(req.Body, codec.UnmarshalOptions{
			StrictUnmarshal: a.Options.StrictUnmarshal,
			ModelStruct:     relation.Relationship().RelatedModelStruct(),
		})
		if err != nil {
			a.marshalErrors(rw, 0, err)
			return
		}

		// Check if none of provided relations has zero value primary key.4
		for _, relation := range payload.Data {
			if relation.IsPrimaryKeyZero() {
				err := httputil.ErrInvalidJSONFieldValue()
				err.Detail = "one of provided relationships doesn't have it's primary key value stored"
				a.marshalErrors(rw, 0, err)
				return
			}
		}

		// Create a query scope.
		s := query.NewScope(mStruct, model)
		s.FieldSets = []mapping.FieldSet{{mStruct.Primary()}}

		// Include relation values.
		if err = s.Include(relation, relation.Relationship().RelatedModelStruct().Primary()); err != nil {
			a.marshalErrors(rw, 500, httputil.ErrInternalError())
			return
		}

		ctx := req.Context()
		modelHandler, hasModelHandler := a.handlers[mStruct]
		if hasModelHandler {
			if w, ok := modelHandler.(server.WithContextUpdateRelationer); ok {
				if ctx, err = w.UpdateRelationsWithContext(ctx); err != nil {
					a.marshalErrors(rw, 0, err)
					return
				}
			}
		}
		// Doing changes in the relationship requires to run it in a transaction.
		tx, err := database.Begin(ctx, a.DB, nil)
		if err != nil {
			a.marshalErrors(rw, 0, err)
			return
		}
		defer func() {
			if err != nil && !tx.State().Done() {
				if err = tx.Rollback(); err != nil {
					log.Errorf("Rolling back a transaction failed")
				}
			}
		}()

		_, err = a.getHandleChain(ctx, tx, s)
		if err != nil {
			a.marshalErrors(rw, 0, err)
			return
		}

		if hasModelHandler {
			if beforeHandler, ok := modelHandler.(server.BeforeUpdateRelationsHandler); ok {
				if err = beforeHandler.HandleBeforeUpdateRelations(ctx, tx, model, payload); err != nil {
					a.marshalErrors(rw, 0, err)
					return
				}
			}
		}

		// Handle set relationships.
		handler, ok := modelHandler.(server.SetRelationsHandler)
		if !ok {
			handler = a.defaultHandler
		}
		var result *codec.Payload
		result, err = handler.HandleSetRelations(ctx, tx, model, payload.Data, relation)
		if err != nil {
			a.marshalErrors(rw, 0, err)
			return
		}

		// Do the after delete handler.
		if hasModelHandler {
			if afterHandler, ok := modelHandler.(server.AfterUpdateRelationsHandler); ok {
				if err = afterHandler.HandleAfterUpdateRelations(ctx, tx, model, payload.Data, result); err != nil {
					a.marshalErrors(rw, 0, err)
					return
				}
			}
		}

		if err = tx.Commit(); err != nil {
			log.Errorf("Cannot commit a transaction: %v", err)
			a.marshalErrors(rw, 500, httputil.ErrInternalError())
			return
		}

		var hasJsonapiMimeType bool
		for _, qv := range httputil.ParseAcceptHeader(req.Header) {
			if qv.Value == jsonapi.MimeType {
				hasJsonapiMimeType = true
				break
			}
		}

		if !hasJsonapiMimeType || result == nil || (result.Data != nil && result.Meta != nil) {
			rw.WriteHeader(http.StatusNoContent)
			return
		}

		link := codec.RelationshipLink
		if !a.Options.PayloadLinks {
			link = codec.NoLink
		}
		result.ModelStruct = relation.Relationship().RelatedModelStruct()
		result.Data = payload.Data
		result.FieldSets = []mapping.FieldSet{{relation.Relationship().RelatedModelStruct().Primary()}}
		result.MarshalLinks = codec.LinkOptions{
			Type:          link,
			BaseURL:       a.Options.PathPrefix,
			RootID:        id,
			Collection:    mStruct.Collection(),
			RelationField: relation.NeuronName(),
		}
		result.MarshalSingularFormat = relation.Kind() == mapping.KindRelationshipSingle
		a.marshalPayload(rw, result, http.StatusOK)
	}
}
