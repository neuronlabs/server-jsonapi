package jsonapi

import (
	"context"

	"github.com/neuronlabs/neuron-extensions/server/http/log"
	"github.com/neuronlabs/neuron/codec"
	"github.com/neuronlabs/neuron/controller"
	"github.com/neuronlabs/neuron/database"
	"github.com/neuronlabs/neuron/errors"
	"github.com/neuronlabs/neuron/mapping"
	"github.com/neuronlabs/neuron/query"
	"github.com/neuronlabs/neuron/server"
)

// DefaultHandler is the default json:api handler. It is used as the default handler in the API.
// The internal fields like 'c' controller would be set by Initialize method.
type DefaultHandler struct {
	c *controller.Controller
}

// Initialize implements controller initializer.
func (d *DefaultHandler) Initialize(c *controller.Controller) error {
	d.c = c
	return nil
}

// HandleInsert implements api.InsertHandler interface.
func (d *DefaultHandler) HandleInsert(ctx context.Context, db database.DB, payload *codec.Payload) (*codec.Payload, error) {
	model := payload.Data[0]
	q := query.NewScope(payload.ModelStruct, model)
	q.FieldSets = payload.FieldSets

	var (
		beganTransaction bool
		err              error
	)
	if len(payload.IncludedRelations) > 0 {
		if _, ok := db.(*database.Tx); !ok {
			beganTransaction = true
			tx, er := database.Begin(ctx, db, nil)
			if er != nil {
				return nil, er
			}
			db = tx
			// if the transaction was create here on error rollback the transaction.
			defer func() {
				if err != nil && !tx.State().Done() {
					if err := tx.Rollback(); err != nil {
						log.Errorf("Rolling back failed: %v", err)
					}
				}
			}()
		}
	}

	// Insert into database.
	inserter := db.(database.QueryInserter)
	if err = inserter.InsertQuery(ctx, q); err != nil {
		log.Debugf("Inserting model to database failed: %v", err)
		return nil, err
	}

	if len(payload.IncludedRelations) == 0 {
		return &codec.Payload{Data: []mapping.Model{model}}, nil
	}

	// Set relation fields.
	for _, relation := range payload.IncludedRelations {
		relationField := relation.StructField
		switch relationField.Relationship().Kind() {
		case mapping.RelBelongsTo:
			continue
		case mapping.RelHasOne:
			// querySetRelations first clear the relationship and then add it - it is not required here as a hasOne
			// only needs to add new relation to it's value.
			single, ok := model.(mapping.SingleRelationer)
			if !ok {
				return nil, errors.WrapDetf(mapping.ErrModelNotImplements, "model: '%s' doesn't implement SingleRelationer interface", payload.ModelStruct)
			}
			// querySetRelations first clear the relationship and then add it - it is not required here as a hasOne
			// only needs to add new relation to it's value.
			var relationModel mapping.Model
			relationModel, err = single.GetRelationModel(relation.StructField)
			if err != nil {
				return nil, err
			}
			if err = db.AddRelations(ctx, model, relation.StructField, relationModel); err != nil {
				return nil, err
			}
		default:
			multi, ok := model.(mapping.MultiRelationer)
			if !ok {
				err = errors.WrapDetf(mapping.ErrModelNotImplements, "model: '%s' doesn't implement MultiRelationer interface", payload.ModelStruct)
				return nil, err
			}
			var relationModels []mapping.Model
			relationModels, err = multi.GetRelationModels(relation.StructField)
			if err != nil {
				return nil, err
			}
			if err = db.SetRelations(ctx, model, relation.StructField, relationModels...); err != nil {
				return nil, err
			}
		}
	}
	if beganTransaction {
		tx := db.(*database.Tx)
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return &codec.Payload{Data: []mapping.Model{model}}, nil
}

// HandleDelete implements api.DeleteHandler interface.
func (d *DefaultHandler) HandleDelete(ctx context.Context, db database.DB, q *query.Scope) (*codec.Payload, error) {
	qdb := db.(database.QueryDeleter)
	deleted, err := qdb.DeleteQuery(ctx, q)
	if err != nil {
		return nil, err
	}
	if deleted == 0 {
		return nil, errors.WrapDetf(query.ErrNoResult, "nothing to delete")
	}
	return nil, nil
}

// HandleUpdate implements api.UpdateHandler interface.
func (d *DefaultHandler) HandleUpdate(ctx context.Context, db database.DB, input *codec.Payload) (*codec.Payload, error) {
	model := input.Data[0]
	var (
		beganTransaction bool
		err              error
	)
	if len(input.IncludedRelations) > 0 {
		if _, ok := db.(*database.Tx); !ok {
			beganTransaction = true
			tx, er := database.Begin(ctx, db, nil)
			if er != nil {
				return nil, er
			}
			db = tx
			// if the transaction was create here on error rollback the transaction.
			defer func() {
				if err != nil {
					if err := tx.Rollback(); err != nil {
						log.Errorf("Rolling back failed: %v", err)
					}
				}
			}()
		}
	}

	// update the model.
	if _, err = db.Update(ctx, input.ModelStruct, model); err != nil {
		return nil, err
	}

	if len(input.IncludedRelations) == 0 {
		return &codec.Payload{Data: []mapping.Model{model}}, nil
	}
	for _, relation := range input.IncludedRelations {
		switch relation.StructField.Relationship().Kind() {
		case mapping.RelHasOne:
			single, ok := model.(mapping.SingleRelationer)
			if !ok {
				err = errors.WrapDetf(mapping.ErrModelNotImplements, "model: '%s' doesn't implement SingleRelationer interface", input.ModelStruct)
				return nil, err
			}
			// querySetRelations first clear the relationship and then add it - it is not required here as a hasOne
			// only needs to add new relation to it's value.
			var relationModel mapping.Model
			relationModel, err = single.GetRelationModel(relation.StructField)
			if err != nil {
				return nil, err
			}
			if err = db.AddRelations(ctx, model, relation.StructField, relationModel); err != nil {
				return nil, err
			}
		default:
			multi, ok := model.(mapping.MultiRelationer)
			if !ok {
				err = errors.WrapDetf(mapping.ErrModelNotImplements, "model: '%s' doesn't implement MultiRelationer interface", input.ModelStruct)
				return nil, err
			}
			var relationModels []mapping.Model
			relationModels, err = multi.GetRelationModels(relation.StructField)
			if err != nil {
				return nil, err
			}
			if err = db.SetRelations(ctx, model, relation.StructField, relationModels...); err != nil {
				return nil, err
			}
		}
	}
	if beganTransaction {
		tx := db.(*database.Tx)
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return &codec.Payload{Data: []mapping.Model{model}}, nil
}

// HandleGet implements api.GetHandler interface.
func (d *DefaultHandler) HandleGet(ctx context.Context, db database.DB, q *query.Scope) (*codec.Payload, error) {
	getter, ok := db.(database.QueryGetter)
	if !ok {
		return nil, errors.WrapDetf(query.ErrInternal, "DB doesn't implement QueryGetter interface: %T", db)
	}
	model, err := getter.QueryGet(ctx, q)
	if err != nil {
		return nil, err
	}
	return &codec.Payload{Data: []mapping.Model{model}}, nil
}

// HandleList implements api.ListHandler interface.
func (d *DefaultHandler) HandleList(ctx context.Context, db database.DB, q *query.Scope) (*codec.Payload, error) {
	finder, ok := db.(database.QueryFinder)
	if !ok {
		return nil, errors.WrapDetf(query.ErrInternal, "DB doesn't implement QueryFinder interface: %T", db)
	}
	models, err := finder.QueryFind(ctx, q)
	if err != nil {
		return nil, err
	}
	return &codec.Payload{Data: models}, nil
}

func (d *DefaultHandler) HandleGetRelation(ctx context.Context, db database.DB, modelQuery, relatedQuery *query.Scope, relation *mapping.StructField) (*codec.Payload, error) {
	getter, ok := db.(database.QueryGetter)
	if !ok {
		return nil, errors.WrapDetf(query.ErrInternal, "DB doesn't implement QueryGetter interface")
	}
	model, err := getter.QueryGet(ctx, modelQuery)
	if err != nil {
		return nil, err
	}

	var (
		payload       codec.Payload
		relatedModels []mapping.Model
	)
	switch relation.Kind() {
	case mapping.KindRelationshipMultiple:
		mr, ok := model.(mapping.MultiRelationer)
		if !ok {
			return nil, errors.WrapDetf(mapping.ErrModelNotImplements, "model: '%s' doesn't implement MultiRelationer", modelQuery.ModelStruct.String())
		}
		relatedModels, err = mr.GetRelationModels(relation)
		if err != nil {
			return nil, err
		}
	case mapping.KindRelationshipSingle:
		sr, ok := model.(mapping.SingleRelationer)
		if !ok {
			return nil, errors.WrapDetf(mapping.ErrModelNotImplements, "model: '%s' doesn't implement SingleRelationer", modelQuery.ModelStruct.String())
		}
		relatedModel, err := sr.GetRelationModel(relation)
		if err != nil {
			return nil, err
		}
		if relatedModel != nil {
			relatedModels = []mapping.Model{relatedModel}
		}
	default:
		return nil, errors.WrapDetf(mapping.ErrInternal, "provided field: '%s' is not a relation", relation.String())
	}

	// Check if there is anything to get from the related scope, or if there are any fields required to be taken from the repository.
	if len(relatedModels) == 0 || relatedQuery == nil || (len(relatedQuery.FieldSets) == 0 && len(relatedQuery.IncludedRelations) == 0) ||
		// Check if the field sets have any other fields than the primary key.
		(len(relatedQuery.FieldSets) == 1 && len(relatedQuery.FieldSets[0]) == 1 && relatedQuery.FieldSets[0][0] == relatedQuery.ModelStruct.Primary()) {
		payload.Data = relatedModels
		return &payload, nil
	}
	relatedQuery.Models = relatedModels
	refresher, ok := db.(database.QueryRefresher)
	if !ok {
		return nil, errors.WrapDetf(query.ErrInternal, "DB doesn't implement QueryRefresher: %T", db)
	}
	if err = refresher.QueryRefresh(ctx, relatedQuery); err != nil {
		return nil, err
	}
	payload.Data = relatedModels
	return &payload, nil
}

// HandleGetRelationship implements GetRelationshipHandler interface.
func (d *DefaultHandler) HandleGetRelationship(ctx context.Context, params server.Params, q *query.Scope, relation *mapping.StructField) (*codec.Payload, error) {
	getter, ok := params.DB.(database.QueryGetter)
	if !ok {
		return nil, errors.WrapDetf(query.ErrInternal, "DB doesn't implement QueryGetter interface: %T", params.DB)
	}
	model, err := getter.QueryGet(ctx, q)
	if err != nil {
		return nil, err
	}

	var payload codec.Payload
	switch relation.Kind() {
	case mapping.KindRelationshipMultiple:
		mr, ok := model.(mapping.MultiRelationer)
		if !ok {
			return nil, errors.WrapDetf(mapping.ErrModelNotImplements, "model: '%s' doesn't implement MultiRelationer", q.ModelStruct.String())
		}
		payload.Data, err = mr.GetRelationModels(relation)
		if err != nil {
			return nil, err
		}
	case mapping.KindRelationshipSingle:
		sr, ok := model.(mapping.SingleRelationer)
		if !ok {
			return nil, errors.WrapDetf(mapping.ErrModelNotImplements, "model: '%s' doesn't implement SingleRelationer", q.ModelStruct.String())
		}
		relatedModel, err := sr.GetRelationModel(relation)
		if err != nil {
			return nil, err
		}
		if relatedModel != nil {
			payload.Data = []mapping.Model{relatedModel}
		}
	default:
		return nil, errors.WrapDetf(mapping.ErrInternal, "provided field: '%s' is not a relation", relation.String())
	}
	return &payload, nil
}

// HandleSetRelations handles the querySetRelations operations by clearing current model's given relation or setting provided 'relationsToSet'.
func (d *DefaultHandler) HandleSetRelations(ctx context.Context, db database.DB, model mapping.Model, relationsToSet []mapping.Model, relation *mapping.StructField) (*codec.Payload, error) {
	q := query.NewScope(relation.ModelStruct(), model)
	if len(relationsToSet) == 0 {
		qrc, ok := db.(database.QueryRelationClearer)
		if !ok {
			return nil, errors.Wrapf(query.ErrInternal, "db doesn't implement QueryRelationClearer: %T", db)
		}
		if _, err := qrc.QueryClearRelations(ctx, q, relation); err != nil {
			return nil, err
		}
		return &codec.Payload{}, nil
	}
	qrs, ok := db.(database.QueryRelationSetter)
	if !ok {
		return nil, errors.Wrapf(query.ErrInternal, "db doesn't implement QueryRelationSetter: %T", db)
	}
	if err := qrs.QuerySetRelations(ctx, q, relation, relationsToSet...); err != nil {
		return nil, err
	}
	return &codec.Payload{}, nil
}
