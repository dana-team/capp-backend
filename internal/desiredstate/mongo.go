package desiredstate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

const objectsCollection = "objects"

const (
	fieldID         = "_id"
	fieldCluster    = "cluster"
	fieldKind       = "kind"
	fieldNamespace  = "namespace"
	fieldGeneration = "generation"
)

// MongoStore keeps records in MongoDB. Every write is a single document, so it
// needs no transactions and works on a standalone server.
type MongoStore struct {
	client  *mongo.Client
	objects *mongo.Collection
}

var _ Store = (*MongoStore)(nil)

// NewMongoStore prepares a store. It does not contact the server: MongoDB being
// down must not keep the backend from serving reads.
func NewMongoStore(uri, database string, timeout time.Duration) (*MongoStore, error) {
	opts := options.Client().
		ApplyURI(uri).
		SetServerSelectionTimeout(timeout).
		SetConnectTimeout(timeout).
		SetWriteConcern(writeconcern.Majority())
	client, err := mongo.Connect(opts)
	if err != nil {
		// The driver's message can repeat the URI, which holds credentials.
		return nil, errors.New("invalid MongoDB connection settings")
	}
	return &MongoStore{client: client, objects: client.Database(database).Collection(objectsCollection)}, nil
}

// EnsureIndexes creates the index that list requests use.
func (s *MongoStore) EnsureIndexes(ctx context.Context) error {
	_, err := s.objects.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: fieldCluster, Value: 1}, {Key: fieldKind, Value: 1}, {Key: fieldNamespace, Value: 1}},
	})
	if err != nil {
		return fmt.Errorf("creating desired state index: %w", err)
	}
	return nil
}

// Close disconnects from the server.
func (s *MongoStore) Close(ctx context.Context) error { return s.client.Disconnect(ctx) }

// Ping implements Store.
func (s *MongoStore) Ping(ctx context.Context) error { return s.client.Ping(ctx, nil) }

// Get implements Store.
func (s *MongoStore) Get(ctx context.Context, ref Ref) (*Record, error) {
	var record Record
	err := s.objects.FindOne(ctx, bson.D{{Key: fieldID, Value: ref.ID()}}).Decode(&record)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading desired state: %w", err)
	}
	return &record, nil
}

// List implements Store.
func (s *MongoStore) List(ctx context.Context, filter Filter) ([]Record, error) {
	query := bson.D{{Key: fieldCluster, Value: filter.Cluster}, {Key: fieldKind, Value: filter.Kind}}
	if filter.Namespaces != nil {
		query = append(query, bson.E{Key: fieldNamespace, Value: bson.D{{Key: "$in", Value: filter.Namespaces}}})
	}
	cursor, err := s.objects.Find(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("listing desired state: %w", err)
	}
	var records []Record
	if err := cursor.All(ctx, &records); err != nil {
		return nil, fmt.Errorf("listing desired state: %w", err)
	}
	return records, nil
}

// Save implements Store.
func (s *MongoStore) Save(ctx context.Context, record *Record, expected int64) error {
	if expected == 0 {
		_, err := s.objects.InsertOne(ctx, record)
		if mongo.IsDuplicateKeyError(err) {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("writing desired state: %w", err)
		}
		return nil
	}
	result, err := s.objects.ReplaceOne(ctx, generationFilter(record.ID, expected), record)
	if err != nil {
		return fmt.Errorf("writing desired state: %w", err)
	}
	if result.MatchedCount == 0 {
		return ErrConflict
	}
	return nil
}

// Delete implements Store.
func (s *MongoStore) Delete(ctx context.Context, ref Ref, expected int64) error {
	result, err := s.objects.DeleteOne(ctx, generationFilter(ref.ID(), expected))
	if err != nil {
		return fmt.Errorf("deleting desired state: %w", err)
	}
	if result.DeletedCount != 0 {
		return nil
	}
	if _, err := s.Get(ctx, ref); errors.Is(err, ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	return ErrConflict
}

// DeleteNamespace implements Store.
func (s *MongoStore) DeleteNamespace(ctx context.Context, cluster, namespace string) error {
	_, err := s.objects.DeleteMany(ctx, bson.D{{Key: fieldCluster, Value: cluster}, {Key: fieldNamespace, Value: namespace}})
	if err != nil {
		return fmt.Errorf("deleting desired state: %w", err)
	}
	return nil
}

func generationFilter(id string, generation int64) bson.D {
	return bson.D{{Key: fieldID, Value: id}, {Key: fieldGeneration, Value: generation}}
}
