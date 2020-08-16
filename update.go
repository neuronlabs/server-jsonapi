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
	"github.com/neuronlabs/neuron/query/filter"
	"github.com/neuronlabs/neuron/server"
)

// HandleUpdate handles json:api list endpoint for the 'model'. Panics if the model is not mapped for given API controller.
func (a *API) HandleUpdate(model mapping.Model) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		a.handleUpdate(a.Controller.MustModelStruct(model))(rw, req)
	}
}

func (a *API) handleUpdate(mStruct *mapping.ModelStruct) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		// Get the id from the url.
		id := httputil.CtxMustGetID(req.Context())
		if id == "" {
			log.Debugf("[PATCH][%s] Empty id params", mStruct.Collection())
			err := httputil.ErrBadRequest()
			err.Detail = "Provided empty 'id' in url"
			a.marshalErrors(rw, 0, err)
			return
		}
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
			err.Detail = "no models found in the input"
			a.marshalErrors(rw, 0, err)
			return
		case 1:
		default:
			err := httputil.ErrInvalidInput()
			err.Detail = "bulk update is not implemented yet"
			a.marshalErrors(rw, 0, err)
			return
		}

		model := payload.Data[0]
		if model.IsPrimaryKeyZero() {
			err = model.SetPrimaryKeyStringValue(id)
		} else {
			unmarshaledID, err := model.GetPrimaryKeyStringValue()
			if err != nil {
				a.marshalErrors(rw, 0, err)
				return
			}
			if unmarshaledID != id {
				err := httputil.ErrInvalidInput()
				err.Detail = "provided input model 'id' differs from the one in the URI"
				log.Debug2f("[PATCH][%s] %s", mStruct.Collection(), err.Detail)
				a.marshalErrors(rw, 0, err)
				return
			}
		}

		unmarshaledFieldset := payload.FieldSets[0]
		relations := mapping.FieldSet{}
		fields := mapping.FieldSet{}
		for _, field := range unmarshaledFieldset {
			switch field.Kind() {
			case mapping.KindRelationshipMultiple, mapping.KindRelationshipSingle:
				// If the relationship is of BelongsTo kind - set its relationship primary key value into given model's foreign key.
				if field.Relationship().Kind() == mapping.RelBelongsTo {
					relationer, ok := model.(mapping.SingleRelationer)
					if !ok {
						log.Errorf("Model: '%s' doesn't implement mapping.SingleRelationer interface", mStruct.Collection())
						a.marshalErrors(rw, 500, httputil.ErrInternalError())
						return
					}
					relation, err := relationer.GetRelationModel(field)
					if err != nil {
						a.marshalErrors(rw, 0, err)
						return
					}
					fielder, ok := model.(mapping.Fielder)
					if !ok {
						log.Errorf("Model: '%s' doesn't implement mapping.SingleRelationer interface", mStruct.Collection())
						a.marshalErrors(rw, 500, httputil.ErrInternalError())
						return
					}
					if err = fielder.SetFieldValue(field.Relationship().ForeignKey(), relation.GetPrimaryKeyValue()); err != nil {
						a.marshalErrors(rw, 0, err)
						return
					}
					fields = append(fields, field.Relationship().ForeignKey())
					continue
				}
				// All the other foreign relations should be post insert.
				relations = append(relations, field)
				continue
			}
			fields = append(fields, field)
		}
		payload.FieldSets[0] = fields
		for _, relation := range relations {
			payload.IncludedRelations = append(payload.IncludedRelations, &query.IncludedRelation{StructField: relation})
		}

		ctx := req.Context()
		db := a.DB
		var (
			isTransactioner bool
			txOpts          *query.TxOptions
		)
		modelHandler, hasModelHandler := a.handlers[mStruct]
		if hasModelHandler {
			if w, ok := modelHandler.(server.WithContextUpdater); ok {
				if ctx, err = w.UpdateWithContext(ctx); err != nil {
					a.marshalErrors(rw, 0, err)
					return
				}
			}

			var t server.UpdateTransactioner
			if t, isTransactioner = modelHandler.(server.UpdateTransactioner); isTransactioner {
				txOpts = t.UpdateWithTransaction()
			}
		}
		if len(relations) > 0 && !isTransactioner {
			isTransactioner = true
		}

		// Get and apply pre hook functions.
		var hasJsonapiMimeType bool
		for _, qv := range httputil.ParseAcceptHeader(req.Header) {
			if qv.Value == jsonapi.MimeType {
				hasJsonapiMimeType = true
				break
			}
		}

		var result *codec.Payload
		if isTransactioner {
			err = database.RunInTransaction(ctx, db, txOpts, func(db database.DB) error {
				result, err = a.fullUpdateHandlerChain(ctx, db, payload, model, hasJsonapiMimeType)
				return err
			})
		} else {
			result, err = a.fullUpdateHandlerChain(ctx, db, payload, model, hasJsonapiMimeType)
		}
		if err != nil {
			a.marshalErrors(rw, 0, err)
			return
		}

		if !hasJsonapiMimeType {
			log.Debug3f("[PATCH][%s] No 'Accept' Header - returning HTTP Status: No Content - 204", mStruct.Collection())
			rw.WriteHeader(http.StatusNoContent)
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
				RootID:     httputil.CtxMustGetID(ctx),
				Collection: mStruct.Collection(),
			}
		}
		result.MarshalSingularFormat = true
		a.marshalPayload(rw, result, http.StatusOK)
	}
}

func (a *API) fullUpdateHandlerChain(ctx context.Context, db database.DB, payload *codec.Payload, model mapping.Model, hasJsonapiMimeType bool) (*codec.Payload, error) {
	result, err := a.updateHandlerChain(ctx, db, payload)
	if err != nil {
		return nil, err
	}
	if !hasJsonapiMimeType {
		return result, nil
	}

	// Prepare the scope for the api.GetHandler.
	mStruct := payload.ModelStruct
	getScope := query.NewScope(mStruct)
	getScope.FieldSets = []mapping.FieldSet{mStruct.Fields()}
	getScope.Filter(filter.New(mStruct.Primary(), filter.OpEqual, model.GetPrimaryKeyValue()))

	for _, relation := range mStruct.RelationFields() {
		if err = getScope.Include(relation, relation.Relationship().RelatedModelStruct().Primary()); err != nil {
			log.Errorf("Can't include relation field to the get scope: %v", err)
			return nil, httputil.ErrInternalError()
		}
	}

	getResult, err := a.getHandleChain(ctx, db, getScope)
	if err != nil {
		return nil, err
	}
	getResult.Meta = result.Meta
	return getResult, nil
}

func (a *API) updateHandlerChain(ctx context.Context, db database.DB, payload *codec.Payload) (*codec.Payload, error) {
	modelHandler, hasModelHandler := a.handlers[payload.ModelStruct]
	// Execute before update hook.
	if hasModelHandler {
		beforeUpdateHandler, ok := modelHandler.(server.BeforeUpdateHandler)
		if ok {
			if err := beforeUpdateHandler.HandleBeforeUpdate(ctx, db, payload); err != nil {
				return nil, err
			}
		}
	}

	updateHandler, ok := modelHandler.(server.UpdateHandler)
	if !ok {
		// If no update handler is found execute default handler.
		updateHandler = a.defaultHandler
	}
	// Execute update handler.
	result, err := updateHandler.HandleUpdate(ctx, db, payload)
	if err != nil {
		return nil, err
	}

	if hasModelHandler {
		afterHandler, ok := modelHandler.(server.AfterUpdateHandler)
		if ok {
			if err = afterHandler.HandleAfterUpdate(ctx, db, result); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
