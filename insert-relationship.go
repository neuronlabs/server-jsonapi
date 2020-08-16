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
	"github.com/neuronlabs/neuron/query/filter"
	"github.com/neuronlabs/neuron/server"
)

// HandleInsertRelationship handles json:api insert relationship endpoint for the 'model'.
// Panics if the model is not mapped for given API controller or the relation doesn't exists.
func (a *API) HandleInsertRelationship(model mapping.Model, relationName string) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		mStruct := a.Controller.MustModelStruct(model)
		relation, ok := mStruct.RelationByName(relationName)
		if !ok {
			panic(fmt.Sprintf("no relation: '%s' found for the model: '%s'", relationName, mStruct.Type().Name()))
		}
		a.handleInsertRelationship(mStruct, relation)(rw, req)
	}
}

func (a *API) handleInsertRelationship(mStruct *mapping.ModelStruct, relation *mapping.StructField) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		// Get the id from the url.
		id := httputil.CtxMustGetID(req.Context())
		if id == "" {
			log.Debugf("[INSERT-RELATIONSHIP][%s] Empty id params", mStruct.Collection())
			err := httputil.ErrBadRequest()
			err.Detail = "Provided empty 'id' in url"
			a.marshalErrors(rw, 0, err)
			return
		}

		model := mapping.NewModel(mStruct)
		if err := model.SetPrimaryKeyStringValue(id); err != nil {
			log.Debugf("[INSERT-RELATIONSHIP][%s] Setting string primary key: %s failed: %v", mStruct, id, err)
			err := httputil.ErrInvalidQueryParameter()
			err.Detail = "provided invalid 'id' in the query parameter."
			a.marshalErrors(rw, 0, err)
			return
		}

		if model.IsPrimaryKeyZero() {
			err := httputil.ErrInvalidQueryParameter()
			err.Detail = "provided zero value primary key"
			a.marshalErrors(rw, 0, err)
			return
		}

		// Unmarshal request input.
		pu := jsonapi.GetCodec(a.Controller).(codec.PayloadUnmarshaler)
		payload, err := pu.UnmarshalPayload(req.Body, codec.UnmarshalOptions{
			StrictUnmarshal: a.Options.StrictUnmarshal,
			ModelStruct:     relation.Relationship().RelatedModelStruct(),
		})
		if err != nil {
			log.Debugf("[INSERT-RELATIONSHIP][%s][%s] unmarshaling payload failed: %v", mStruct, relation, err)
			a.marshalErrors(rw, 0, err)
			return
		}
		if relation.Kind() == mapping.KindRelationshipSingle && len(payload.Data) > 1 {
			log.Debugf("[INSERT-RELATIONSHIP][%s][%s] to-one relationship has more than one input", mStruct, relation)
			err := httputil.ErrInvalidInput()
			err.Detail = "cannot set many relationships for a to-one relationship"
			a.marshalErrors(rw, 0, err)
			return
		}

		// Check if none of provided relations has zero value primary key.
		for _, relation := range payload.Data {
			if relation.IsPrimaryKeyZero() {
				err := httputil.ErrInvalidJSONFieldValue()
				err.Detail = "one of provided relationships doesn't have it's primary key value stored"
				a.marshalErrors(rw, 0, err)
				return
			}
		}

		if len(payload.Data) == 0 {
			rw.WriteHeader(http.StatusNoContent)
			return
		}

		s := query.NewScope(mStruct)
		s.FieldSets = payload.FieldSets
		s.Filter(filter.New(mStruct.Primary(), filter.OpEqual, model.GetPrimaryKeyValue()))

		// Include relation values.
		if err = s.Include(relation, relation.Relationship().RelatedModelStruct().Primary()); err != nil {
			log.Errorf("[INSERT-RELATIONSHIP][%s][%s] including relation with it's primary key failed: %v", mStruct, relation, err)
			a.marshalErrors(rw, 500, httputil.ErrInternalError())
			return
		}

		ctx := req.Context()
		modelHandler, hasModelHandler := a.handlers[mStruct]
		if hasModelHandler {
			if w, ok := modelHandler.(server.WithContextInsertRelationer); ok {
				if ctx, err = w.InsertRelationsWithContext(ctx); err != nil {
					a.marshalErrors(rw, 0, err)
					return
				}
			}
		}

		// Doing changes in the relationship requires to run it in a transaction.
		tx, err := database.Begin(ctx, a.DB, nil)
		if err != nil {
			log.Errorf("[INSERT-RELATIONSHIP][%s][%s] begin transaction failed: %v", mStruct, relation, err)
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
			log.Debugf("[INSERT-RELATIONSHIP][%s][%s] getting model with included relationship failed: %v", mStruct, relation, err)
			a.marshalErrors(rw, 0, err)
			return
		}

		if hasModelHandler {
			if beforeHandler, ok := modelHandler.(server.BeforeInsertRelationsHandler); ok {
				if err = beforeHandler.HandleBeforeInsertRelations(ctx, tx, model, payload); err != nil {
					a.marshalErrors(rw, 0, err)
					return
				}
			}
		}

		var relationModels []mapping.Model
		switch relation.Kind() {
		case mapping.KindRelationshipMultiple:
			mr, ok := model.(mapping.MultiRelationer)
			if !ok {
				log.Errorf("[INSERT-RELATIONSHIP][%s][%s] model doesn't implement MultiRelationer interface", mStruct, relation)
				err = httputil.ErrInternalError()
				a.marshalErrors(rw, 500, httputil.ErrInternalError())
				return
			}
			var models []mapping.Model
			models, err = mr.GetRelationModels(relation)
			if err != nil {
				log.Errorf("[INSERT-RELATIONSHIP][%s][%s] getting MultiRelationer relations failed: %v", mStruct, relation, err)
				a.marshalErrors(rw, 0, err)
				return
			}
			for _, relationModel := range models {
				if relationModel != nil {
					relationModels = append(relationModels, relationModel)
				}
			}
		case mapping.KindRelationshipSingle:
			sr, ok := model.(mapping.SingleRelationer)
			if !ok {
				log.Errorf("[INSERT-RELATIONSHIP][%s][%s] model doesn't implement SingleRelationer interface", mStruct, relation)
				err = httputil.ErrInternalError()
				a.marshalErrors(rw, 500, httputil.ErrInternalError())
				return
			}
			var relationModel mapping.Model
			relationModel, err = sr.GetRelationModel(relation)
			if err != nil {
				log.Errorf("[INSERT-RELATIONSHIP][%s][%s] getting SingleRelationer models failed: %v", mStruct, relation, err)
				a.marshalErrors(rw, 0, err)
				return
			}
			if relationModel != nil {
				relationModels = append(relationModels, relationModel)
			}
		}

		// Get the set of (current relations) - (to delete relations)  -> relations to set.
		idMap := map[interface{}]int{}
		relationsToSet := relationModels
		for i, current := range relationModels {
			idMap[current.GetPrimaryKeyHashableValue()] = i
		}

		for _, toInsert := range payload.Data {
			_, ok := idMap[toInsert.GetPrimaryKeyHashableValue()]
			if ok {
				continue
			}
			relationsToSet = append(relationsToSet, toInsert)
		}

		// If nothing is being deleted - json:api specify that this is successful request - and return no content status.
		if len(relationsToSet) == len(relationModels) {
			if err = tx.Commit(); err != nil {
				log.Errorf("Committing transaction failed: %v", err)
			}
			rw.WriteHeader(http.StatusNoContent)
			return
		}

		handler, ok := modelHandler.(server.SetRelationsHandler)
		if !ok {
			handler = a.defaultHandler
		}

		var result *codec.Payload
		result, err = handler.HandleSetRelations(ctx, tx, model, relationsToSet, relation)
		if err != nil {
			log.Debugf("[INSERT-RELATIONSHIPS][%s][%S] HandleSetRelations failed: %v", err)
			a.marshalErrors(rw, 0, err)
			return
		}
		if hasModelHandler {
			if afterHandler, ok := modelHandler.(server.AfterInsertRelationsHandler); ok {
				if err = afterHandler.HandleAfterInsertRelations(ctx, tx, model, relationsToSet, result); err != nil {
					a.marshalErrors(rw, 0, err)
					return
				}
			}
		}

		if err = tx.Commit(); err != nil {
			log.Errorf("Committing transaction failed: %v", err)
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
