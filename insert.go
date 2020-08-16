package jsonapi

import (
	"context"
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

// HandleInsert handles json:api post endpoint for the 'model'. Panics if the model is not mapped for given API controller.
func (a *API) HandleInsert(model mapping.Model) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		a.handleInsert(a.Controller.MustModelStruct(model))(rw, req)
	}
}

func (a *API) handleInsert(mStruct *mapping.ModelStruct) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		// unmarshal the input from the request body.
		pu := jsonapi.GetCodec(a.Controller).(codec.PayloadUnmarshaler)
		payload, err := pu.UnmarshalPayload(req.Body, codec.UnmarshalOptions{StrictUnmarshal: a.Options.StrictUnmarshal, ModelStruct: mStruct})
		if err != nil {
			log.Debugf("Unmarshal scope for: '%s' failed: %v", mStruct.Collection(), err)
			a.marshalErrors(rw, 0, err)
			return
		}

		switch len(payload.Data) {
		case 0:
			err := httputil.ErrInvalidInput()
			err.Detail = "nothing to insert"
			a.marshalErrors(rw, 0, err)
			return
		case 1:
		default:
			err := httputil.ErrInvalidInput()
			err.Detail = "bulk insert not implemented yet."
			a.marshalErrors(rw, 0, err)
			return
		}
		model := payload.Data[0]

		// Divide fieldset into fields and relations.
		if len(payload.FieldSets) != 1 {
			err := httputil.ErrInvalidInput()
			err.Detail = "bulk inserted not implemented yet"
			a.marshalErrors(rw, 0, err)
			return
		}

		var selectedPrimary bool
		fields := mapping.FieldSet{}
		for _, field := range payload.FieldSets[0] {
			switch field.Kind() {
			case mapping.KindRelationshipSingle, mapping.KindRelationshipMultiple:
				if field.Relationship().Kind() == mapping.RelBelongsTo {
					relationer, ok := model.(mapping.SingleRelationer)
					if !ok {
						log.Errorf("Model: '%s' doesn't implement mapping.SingleRelationer interface", mStruct.Collection())
						a.marshalErrors(rw, 500, httputil.ErrInternalError())
						return
					}
					relation, err := relationer.GetRelationModel(field)
					if err != nil {
						log.Errorf("Getting relation model failed: %v", err)
						a.marshalErrors(rw, 500, httputil.ErrInternalError())
						return
					}
					if relation.IsPrimaryKeyZero() {
						a.marshalErrors(rw, http.StatusBadRequest, httputil.ErrInvalidQueryParameter())
						return
					}

					fielder, ok := model.(mapping.Fielder)
					if !ok {
						log.Errorf("Model: '%s' doesn't implement mapping.Fielder interface", mStruct.Collection())
						a.marshalErrors(rw, 500, httputil.ErrInternalError())
					}
					foreignKey := field.Relationship().ForeignKey()
					if err = fielder.SetFieldValue(foreignKey, relation.GetPrimaryKeyValue()); err != nil {
						log.Errorf("Setting relation foreign key value failed: %v", err)
						a.marshalErrors(rw, 500, httputil.ErrInternalError())
						return
					}
					if !fields.Contains(foreignKey) {
						fields = append(fields, foreignKey)
					}
				}
				payload.IncludedRelations = append(payload.IncludedRelations, &query.IncludedRelation{
					StructField: field,
				})
			case mapping.KindPrimary:
				fields = append(fields, field)
				selectedPrimary = true
			case mapping.KindAttribute:
				fields = append(fields, field)
			}
		}
		payload.FieldSets = []mapping.FieldSet{fields}

		// Check if a model is allowed to set it's primary key.
		if selectedPrimary && !mStruct.AllowClientID() {
			log.Debug2f("Creating: '%s' with client-generated ID is forbidden", mStruct.Collection())
			err := httputil.ErrInvalidJSONFieldValue()
			err.Detail = "Client-Generated ID is not allowed for this model."
			err.Status = "403"
			a.marshalErrors(rw, http.StatusForbidden, err)
			return
		}

		// Prepare parameters.
		ctx := req.Context()
		db := a.DB
		var (
			result          *codec.Payload
			isTransactioner bool
		)

		// Try to get model's InsertHandler.
		modelHandler, hasModelHandler := a.handlers[mStruct]

		if hasModelHandler {
			if w, ok := modelHandler.(server.WithContextInserter); ok {
				if ctx, err = w.InsertWithContext(ctx); err != nil {
					a.marshalErrors(rw, 0, err)
					return
				}
			}

			var it server.InsertTransactioner
			if it, isTransactioner = modelHandler.(server.InsertTransactioner); isTransactioner {
				err = database.RunInTransaction(ctx, db, it.InsertWithTransaction(), func(db database.DB) error {
					result, err = a.insertHandleChain(ctx, db, payload)
					return err
				})
			}
		}

		if !isTransactioner {
			result, err = a.insertHandleChain(ctx, db, payload)
		}
		if err != nil {
			a.marshalErrors(rw, 0, err)
			return
		}

		// if the primary was provided in the input and if the config doesn't allow to return
		// created value with given client-id - return simple status NoContent
		if selectedPrimary && a.Options.NoContentOnInsert {
			// if the primary was provided
			rw.WriteHeader(http.StatusNoContent)
			return
		}
		if len(result.Data) == 0 {
			log.Error("No data in the result payload")
			a.marshalErrors(rw, 500, httputil.ErrInternalError())
			return
		}

		// get the primary field value so that it could be used for the jsonapi marshal process.
		stringID, err := model.GetPrimaryKeyStringValue()
		if err != nil {
			log.Errorf("Getting primary key string value failed for the model: %v", model)
			a.marshalErrors(rw, 500, httputil.ErrInternalError())
			return
		}

		linkType := codec.ResourceLink
		// but if the config doesn't allow that - set 'jsonapi.NoLink'
		if !a.Options.PayloadLinks {
			linkType = codec.NoLink
		}

		result.ModelStruct = mStruct
		result.FieldSets = []mapping.FieldSet{append(mStruct.Fields(), mStruct.RelationFields()...)}
		if result.MarshalLinks.Type == codec.NoLink {
			result.MarshalLinks = codec.LinkOptions{
				Type:       linkType,
				BaseURL:    a.Options.PathPrefix,
				RootID:     stringID,
				Collection: mStruct.Collection(),
			}
		}
		result.MarshalSingularFormat = true
		a.marshalPayload(rw, result, http.StatusCreated)
	}
}

func (a *API) insertHandleChain(ctx context.Context, db database.DB, payload *codec.Payload) (*codec.Payload, error) {
	modelHandler, hasModelHandler := a.handlers[payload.ModelStruct]
	if hasModelHandler {
		beforeInserter, ok := modelHandler.(server.BeforeInsertHandler)
		if ok {
			if err := beforeInserter.HandleBeforeInsert(ctx, db, payload); err != nil {
				return nil, err
			}
		}
	}
	insertHandler, ok := modelHandler.(server.InsertHandler)
	if !ok {
		// If nothing is being found take the default handler.
		insertHandler = a.defaultHandler
	}

	result, err := insertHandler.HandleInsert(ctx, db, payload)
	if err != nil {
		log.Debugf("Handle insert failed: %v", err)
		return nil, err
	}

	if hasModelHandler {
		afterHandler, ok := modelHandler.(server.AfterInsertHandler)
		if ok {
			if err = afterHandler.HandleAfterInsert(ctx, db, result); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
