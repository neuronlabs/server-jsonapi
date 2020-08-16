package jsonapi

import (
	"context"
	"net/http"

	"github.com/neuronlabs/neuron/codec"
	"github.com/neuronlabs/neuron/database"
	"github.com/neuronlabs/neuron/mapping"
	"github.com/neuronlabs/neuron/query"
	"github.com/neuronlabs/neuron/server"

	"github.com/neuronlabs/neuron-extensions/server/http/httputil"
	"github.com/neuronlabs/neuron-extensions/server/http/log"
)

// HandleDelete handles json:api delete endpoint for the 'model'. Panics if the model is not mapped for given API controller.
func (a *API) HandleDelete(model mapping.Model) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		a.handleDelete(a.Controller.MustModelStruct(model))(rw, req)
	}
}

func (a *API) handleDelete(mStruct *mapping.ModelStruct) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		id := httputil.CtxMustGetID(ctx)
		if id == "" {
			// if the function would not contain 'id' parameter.
			log.Debugf("[DELETE] Empty id params: %v", id)
			err := httputil.ErrInvalidQueryParameter()
			err.Detail = "Provided empty id in the query URL"
			a.marshalErrors(rw, 0, err)
			return
		}

		model := mapping.NewModel(mStruct)
		err := model.SetPrimaryKeyStringValue(id)
		if err != nil {
			log.Debugf("[DELETE][%s] Invalid URL id value: '%s': '%v'", mStruct.Collection(), id, err)
			a.marshalErrors(rw, 0, err)
			return
		}

		// Check if the primary key is not zero value.
		if model.IsPrimaryKeyZero() {
			err := httputil.ErrInvalidQueryParameter()
			err.Detail = "provided zero value primary key for the model"
			a.marshalErrors(rw, 0, err)
			return
		}
		// Create scope for the delete purpose.
		s := query.NewScope(mStruct, model)

		db := a.DB

		modelHandler, hasModelHandler := a.handlers[mStruct]
		if hasModelHandler {
			if ctxSetter, ok := modelHandler.(server.WithContextDeleter); ok {
				if ctx, err = ctxSetter.DeleteWithContext(ctx); err != nil {
					a.marshalErrors(rw, 0, err)
					return
				}
			}
		}

		var (
			result          *codec.Payload
			isTransactioner bool
		)

		if hasModelHandler {
			var transactioner server.DeleteTransactioner
			if transactioner, isTransactioner = modelHandler.(server.DeleteTransactioner); isTransactioner {
				err = database.RunInTransaction(ctx, db, transactioner.DeleteWithTransaction(), func(tx database.DB) error {
					result, err = a.deleteHandlerChain(ctx, tx, s)
					return err
				})
			}
		}
		if !isTransactioner {
			result, err = a.deleteHandlerChain(ctx, db, s)
		}
		if err != nil {
			a.marshalErrors(rw, 0, err)
			return
		}

		if result == nil || result.Meta == nil {
			// Write no content status.
			rw.WriteHeader(http.StatusNoContent)
			return
		}
		a.marshalPayload(rw, result, http.StatusOK)
	}
}

func (a *API) deleteHandlerChain(ctx context.Context, db database.DB, s *query.Scope) (*codec.Payload, error) {
	modelHandler, hasModelHandler := a.handlers[s.ModelStruct]

	// Handle before delete hook.
	if hasModelHandler {
		beforeDeleter, ok := modelHandler.(server.BeforeDeleteHandler)
		if ok {
			if err := beforeDeleter.HandleBeforeDelete(ctx, db, s); err != nil {
				return nil, err
			}
		}
	}

	deleteHandler, ok := modelHandler.(server.DeleteHandler)
	if !ok {
		deleteHandler = a.defaultHandler
	}

	// Handle delete.
	result, err := deleteHandler.HandleDelete(ctx, db, s)
	if err != nil {
		log.Debugf("[DELETE][SCOPE][%s] Delete %s failed: %v", s.ID, s.ModelStruct.Collection(), err)
		return nil, err
	}

	// Handle after delete hooks.
	if hasModelHandler {
		afterHandler, ok := modelHandler.(server.AfterDeleteHandler)
		if ok {
			if err = afterHandler.HandleAfterDelete(ctx, db, s, result); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
