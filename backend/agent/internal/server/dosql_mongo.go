package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MongoDB statements are JSON operation documents instead of SQL text:
//
//	{"collection":"orders","op":"updateMany","filter":{"status":"new"},"update":{"$set":{"status":"paid"}}}
//
// A batch is a JSON array or several documents in a row. Every operation is
// validated while planning, so the fingerprint, version journal and executor all
// see the same canonical text. Data operations run in one transaction (replica
// set required); createCollection/createIndex/dropIndex/drop are not
// transactional, so they follow the same implicit-commit rules as MySQL DDL.
var (
	mongoCollectionName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,119}$`)
	// Server-side JavaScript would let a "data" operation run arbitrary code.
	mongoForbiddenOperators = []string{"$where", "$function", "$accumulator"}
)

type mongoOperation struct {
	Collection  string          `json:"collection"`
	Op          string          `json:"op"`
	Filter      json.RawMessage `json:"filter,omitempty"`
	Sort        json.RawMessage `json:"sort,omitempty"`
	Projection  json.RawMessage `json:"projection,omitempty"`
	Limit       int64           `json:"limit,omitempty"`
	Pipeline    json.RawMessage `json:"pipeline,omitempty"`
	Document    json.RawMessage `json:"document,omitempty"`
	Documents   json.RawMessage `json:"documents,omitempty"`
	Update      json.RawMessage `json:"update,omitempty"`
	Replacement json.RawMessage `json:"replacement,omitempty"`
	Upsert      bool            `json:"upsert,omitempty"`
	Keys        json.RawMessage `json:"keys,omitempty"`
	Name        string          `json:"name,omitempty"`
	Unique      bool            `json:"unique,omitempty"`

	To                 string          `json:"to,omitempty"`
	Field              string          `json:"field,omitempty"`
	Return             string          `json:"return,omitempty"`
	Verbosity          string          `json:"verbosity,omitempty"`
	Options            json.RawMessage `json:"options,omitempty"`
	ExpireAfterSeconds *int32          `json:"expireAfterSeconds,omitempty"`
	Sparse             bool            `json:"sparse,omitempty"`
	PartialFilter      json.RawMessage `json:"partialFilter,omitempty"`
	OpID               *int64          `json:"opid,omitempty"`
	ConfirmDatabase    string          `json:"confirmDatabase,omitempty"`
}

// Kinds: read runs in query mode; write joins the transaction; ddl covers every
// operation MongoDB cannot make transactional (collection, index, database and
// server administration), so it follows the implicit-commit rules.
var mongoOpKinds = map[string]string{
	"find": "read", "count": "read", "aggregate": "read", "distinct": "read", "explain": "read",
	"listDatabases": "read", "listCollections": "read", "listIndexes": "read",
	"dbStats": "read", "collStats": "read", "currentOp": "read", "serverStatus": "read",
	"replSetStatus": "read", "buildInfo": "read",
	"insertOne": "write", "insertMany": "write", "updateOne": "write", "updateMany": "write",
	"replaceOne": "write", "deleteOne": "write", "deleteMany": "write",
	"findOneAndUpdate": "write", "findOneAndReplace": "write", "findOneAndDelete": "write",
	"createCollection": "ddl", "createIndex": "ddl", "dropIndex": "ddl", "drop": "ddl",
	"renameCollection": "ddl", "collMod": "ddl", "compact": "ddl", "killOp": "ddl", "dropDatabase": "ddl",
}

// Database or server level operations do not name a collection.
var mongoDatabaseLevelOps = map[string]bool{
	"listDatabases": true, "listCollections": true, "dbStats": true, "currentOp": true,
	"serverStatus": true, "replSetStatus": true, "buildInfo": true, "killOp": true, "dropDatabase": true,
}

// Administrative reads cannot run inside a transaction, so they always use the
// plain context and never see uncommitted writes of the same batch.
func mongoNeedsPlainContext(op string) bool {
	switch op {
	case "listDatabases", "listCollections", "listIndexes", "dbStats", "collStats", "currentOp",
		"serverStatus", "replSetStatus", "buildInfo", "explain", "distinct":
		return true
	}
	return false
}

func parseMongoOperation(statement string) (mongoOperation, error) {
	var op mongoOperation
	decoder := json.NewDecoder(strings.NewReader(statement))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&op); err != nil {
		return op, fmt.Errorf("dosql_mongo_operation_invalid: %v", err)
	}
	if _, kind := mongoOpKinds[op.Op]; !kind {
		return op, fmt.Errorf("dosql_mongo_operation_invalid: unknown op %q; use %s", op.Op, strings.Join(mongoOpNames(), ", "))
	}
	if !mongoDatabaseLevelOps[op.Op] || op.Collection != "" {
		if !mongoCollectionName.MatchString(op.Collection) || strings.HasPrefix(op.Collection, "system.") {
			return op, errors.New("dosql_mongo_operation_invalid: collection name is missing or not allowed")
		}
	}
	for _, raw := range [][]byte{op.Filter, op.Sort, op.Projection, op.Pipeline, op.Document, op.Documents, op.Update, op.Replacement, op.Keys, op.Options, op.PartialFilter} {
		if len(raw) == 0 {
			continue
		}
		if err := validateMongoDocumentKeys(raw); err != nil {
			return op, err
		}
	}
	if op.Limit < 0 {
		return op, errors.New("dosql_mongo_operation_invalid: limit must not be negative")
	}
	switch op.Op {
	case "insertOne":
		if len(op.Document) == 0 {
			return op, errors.New("dosql_mongo_operation_invalid: insertOne needs document")
		}
	case "insertMany":
		if len(op.Documents) == 0 {
			return op, errors.New("dosql_mongo_operation_invalid: insertMany needs documents")
		}
	case "updateOne", "updateMany":
		if len(op.Update) == 0 || len(op.Filter) == 0 {
			return op, fmt.Errorf("dosql_mongo_operation_invalid: %s needs filter and update", op.Op)
		}
	case "replaceOne":
		if len(op.Replacement) == 0 || len(op.Filter) == 0 {
			return op, errors.New("dosql_mongo_operation_invalid: replaceOne needs filter and replacement")
		}
	case "deleteOne", "deleteMany":
		if len(op.Filter) == 0 {
			return op, fmt.Errorf("dosql_mongo_operation_invalid: %s needs filter; pass {} to match every document", op.Op)
		}
	case "aggregate":
		if len(op.Pipeline) == 0 {
			return op, errors.New("dosql_mongo_operation_invalid: aggregate needs pipeline")
		}
		var stages []map[string]json.RawMessage
		if err := json.Unmarshal(op.Pipeline, &stages); err != nil {
			return op, errors.New("dosql_mongo_operation_invalid: pipeline must be an array of stages")
		}
		for _, stage := range stages {
			for key := range stage {
				if key == "$out" || key == "$merge" {
					return op, fmt.Errorf("dosql_mongo_stage_unsupported: %s writes outside the transaction; use an explicit write operation", key)
				}
			}
		}
	case "createIndex":
		if len(op.Keys) == 0 {
			return op, errors.New("dosql_mongo_operation_invalid: createIndex needs keys")
		}
		if op.ExpireAfterSeconds != nil && *op.ExpireAfterSeconds < 0 {
			return op, errors.New("dosql_mongo_operation_invalid: expireAfterSeconds must not be negative")
		}
	case "dropIndex":
		if op.Name == "" || op.Name == "*" {
			return op, errors.New("dosql_mongo_operation_invalid: dropIndex needs one index name")
		}
	case "findOneAndUpdate":
		if len(op.Update) == 0 || len(op.Filter) == 0 {
			return op, errors.New("dosql_mongo_operation_invalid: findOneAndUpdate needs filter and update")
		}
	case "findOneAndReplace":
		if len(op.Replacement) == 0 || len(op.Filter) == 0 {
			return op, errors.New("dosql_mongo_operation_invalid: findOneAndReplace needs filter and replacement")
		}
	case "findOneAndDelete":
		if len(op.Filter) == 0 {
			return op, errors.New("dosql_mongo_operation_invalid: findOneAndDelete needs filter")
		}
	case "distinct":
		if op.Field == "" {
			return op, errors.New("dosql_mongo_operation_invalid: distinct needs field")
		}
	case "explain":
		switch op.Verbosity {
		case "", "queryPlanner", "executionStats", "allPlansExecution":
		default:
			return op, errors.New("dosql_mongo_operation_invalid: verbosity must be queryPlanner, executionStats or allPlansExecution")
		}
	case "renameCollection":
		if !mongoCollectionName.MatchString(op.To) || strings.HasPrefix(op.To, "system.") || op.To == op.Collection {
			return op, errors.New("dosql_mongo_operation_invalid: renameCollection needs a different, allowed target name in to")
		}
	case "collMod":
		if len(bytes.TrimSpace(op.Options)) < 3 {
			return op, errors.New("dosql_mongo_operation_invalid: collMod needs options")
		}
	case "killOp":
		if op.OpID == nil {
			return op, errors.New("dosql_mongo_operation_invalid: killOp needs opid from currentOp")
		}
	case "dropDatabase":
		if op.ConfirmDatabase == "" {
			return op, errors.New("dosql_mongo_operation_invalid: dropDatabase needs confirmDatabase set to the database name")
		}
	}
	switch op.Return {
	case "", "before", "after":
	default:
		return op, errors.New("dosql_mongo_operation_invalid: return must be before or after")
	}
	return op, nil
}

func mongoOpNames() []string {
	names := make([]string, 0, len(mongoOpKinds))
	for name := range mongoOpKinds {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// parseMongoStatements turns a JSON array (or consecutive JSON documents) into
// canonical single-line operations.
func parseMongoStatements(text string) ([]string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("dosql_statement_required")
	}
	var raws []json.RawMessage
	if strings.HasPrefix(text, "[") {
		if err := json.Unmarshal([]byte(text), &raws); err != nil {
			return nil, fmt.Errorf("dosql_mongo_operation_invalid: %v", err)
		}
	} else {
		decoder := json.NewDecoder(strings.NewReader(text))
		for decoder.More() {
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err != nil {
				return nil, fmt.Errorf("dosql_mongo_operation_invalid: %v", err)
			}
			raws = append(raws, raw)
		}
	}
	if len(raws) == 0 {
		return nil, errors.New("dosql_statement_required")
	}
	if len(raws) > doSQLMaxStatements {
		return nil, errors.New("dosql_statement_batch_too_large")
	}
	statements := make([]string, 0, len(raws))
	for index, raw := range raws {
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return nil, fmt.Errorf("dosql_mongo_operation_invalid: operation %d: %v", index+1, err)
		}
		if _, err := parseMongoOperation(compact.String()); err != nil {
			return nil, fmt.Errorf("%v (operation %d)", err, index+1)
		}
		statements = append(statements, compact.String())
	}
	return statements, nil
}

func mongoOperationKind(statement string) string {
	op, err := parseMongoOperation(statement)
	if err != nil {
		return ""
	}
	return mongoOpKinds[op.Op]
}

// Walk the decoded keys as tokens so duplicate fields cannot hide an earlier value.
func validateMongoDocumentKeys(raw json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var visit func() error
	visit = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		for decoder.More() {
			if delimiter == '{' {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				for _, forbidden := range mongoForbiddenOperators {
					if key == forbidden {
						return fmt.Errorf("dosql_mongo_javascript_forbidden: %s runs server-side JavaScript", forbidden)
					}
				}
			}
			if err := visit(); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	return visit()
}

func mongoBSON(raw json.RawMessage) (bson.D, error) {
	doc := bson.D{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return doc, nil
	}
	if err := bson.UnmarshalExtJSON(raw, false, &doc); err != nil {
		return nil, fmt.Errorf("dosql_mongo_document_invalid: %v", err)
	}
	return doc, nil
}

func mongoBSONList(raw json.RawMessage) ([]bson.D, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return nil, errors.New("dosql_mongo_document_invalid: expected a non-empty array of documents")
	}
	docs := make([]bson.D, 0, len(items))
	for _, item := range items {
		doc, err := mongoBSON(item)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

func mongoDocumentText(doc interface{}) string {
	encoded, err := bson.MarshalExtJSON(doc, false, false)
	if err != nil {
		return "<unencodable document>"
	}
	return redactSecretText(string(encoded))
}

func mongoConnectURI(raw, authSource string) string {
	if authSource == "" {
		return raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := parsed.Query()
	query.Set("authSource", authSource)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// connectDoSQLMongo authenticates against the URI's own database first. Root
// users usually live in "admin", so an authentication failure is retried once
// there; the source that worked is reported, never guessed silently.
func connectDoSQLMongo(ctx context.Context, rawURI string) (*mongo.Client, string, error) {
	attempts := []string{""}
	if parsed, err := url.Parse(rawURI); err == nil && parsed.Query().Get("authSource") == "" {
		attempts = append(attempts, "admin")
	}
	var lastErr error
	for _, source := range attempts {
		client, err := mongo.Connect(options.Client().ApplyURI(mongoConnectURI(rawURI, source)).SetServerSelectionTimeout(10 * time.Second))
		if err != nil {
			return nil, "", doSQLFailure("dosql_connect_failed", err.Error())
		}
		if err := client.Ping(ctx, nil); err != nil {
			_ = client.Disconnect(ctx)
			lastErr = err
			if strings.Contains(strings.ToLower(err.Error()), "auth") {
				continue
			}
			return nil, "", doSQLFailure("dosql_connect_failed", err.Error())
		}
		return client, source, nil
	}
	return nil, "", doSQLFailure("dosql_connect_failed", lastErr.Error())
}

// executeDoSQLMongo mirrors executeDoSQLDirect: data operations plus the
// verification share one transaction and roll back together; non-transactional
// operations are counted as implicit commits.
func executeDoSQLMongo(ctx context.Context, request doSQLDirectRequest) (doSQLDirectResult, error) {
	started := time.Now()
	result := doSQLDirectResult{Status: "failed"}
	ctx, cancel := context.WithTimeout(ctx, doSQLDirectStatementT)
	defer cancel()
	operations := make([]mongoOperation, 0, len(request.Statements))
	hasData := false
	for _, statement := range request.Statements {
		op, err := parseMongoOperation(statement)
		if err != nil {
			return result, err
		}
		if op.Op == "dropDatabase" && op.ConfirmDatabase != request.Target.Database {
			result.Status = "rejected"
			return result, fmt.Errorf("dosql_mongo_confirm_mismatch: confirmDatabase %q does not match the target database %q", op.ConfirmDatabase, request.Target.Database)
		}
		kind := mongoOpKinds[op.Op]
		if request.ReadOnly && kind != "read" {
			result.Status = "rejected"
			return result, errors.New("dosql_query_requires_read_only: only read operations (find, count, aggregate, distinct, explain, list*, stats, currentOp, serverStatus) run in query mode")
		}
		hasData = hasData || kind == "write"
		operations = append(operations, op)
	}
	if !request.ReadOnly {
		// Collection-level DDL cannot join a transaction, so it can neither be
		// dry-run nor share a batch with writes. Refused before connecting.
		firstDDL, firstWrite := 0, 0
		for index, op := range operations {
			switch mongoOpKinds[op.Op] {
			case "ddl":
				if firstDDL == 0 {
					firstDDL = index + 1
				}
			case "write":
				if firstWrite == 0 {
					firstWrite = index + 1
				}
			}
		}
		if request.DryRun && firstDDL != 0 {
			result.Status = "rejected"
			return result, fmt.Errorf("dosql_dry_run_ddl_unsupported: statement %d is a collection-level operation that MongoDB cannot roll back, so a dry run would change data; apply it for real", firstDDL)
		}
		if firstDDL != 0 && firstWrite != 0 {
			result.Status = "rejected"
			return result, fmt.Errorf("dosql_mixed_ddl_dml_unsupported: statement %d is collection-level and statement %d is a write; MongoDB cannot make them atomic, apply them as separate operations", firstDDL, firstWrite)
		}
	}
	client, authSource, err := connectDoSQLMongo(ctx, request.Target.DSN)
	if err != nil {
		return result, err
	}
	defer client.Disconnect(context.Background())
	result.AuthSource = authSource
	database := client.Database(request.Target.Database)

	runCtx := ctx
	var session *mongo.Session
	transactionOpen := false
	if hasData && !request.ReadOnly {
		session, err = client.StartSession()
		if err != nil {
			return result, doSQLFailure("dosql_transaction_failed", err.Error())
		}
		defer session.EndSession(context.Background())
		if err := session.StartTransaction(); err != nil {
			return result, doSQLFailure("dosql_transaction_failed", err.Error())
		}
		transactionOpen = true
		runCtx = mongo.NewSessionContext(ctx, session)
		defer func() {
			if transactionOpen {
				_ = session.AbortTransaction(context.Background())
			}
		}()
	}
	failure := func(category string, err error) error {
		if strings.Contains(err.Error(), "Transaction numbers are only allowed") {
			return errors.New("dosql_transaction_unsupported: this MongoDB is a standalone server; atomic multi-operation writes and dry runs need a replica set, nothing was committed")
		}
		return doSQLFailure(category, err.Error()+partialCommitNote(&result))
	}
	for index, op := range operations {
		opCtx := runCtx
		if mongoNeedsPlainContext(op.Op) {
			opCtx = ctx
		}
		executed, err := runDoSQLMongoOperation(opCtx, database, op, request.Statements[index])
		if err != nil {
			return result, failure("dosql_statement_failed", fmt.Errorf("statement %d: %v", index+1, err))
		}
		result.Statements = append(result.Statements, executed)
		if mongoOpKinds[op.Op] == "ddl" {
			result.ImplicitCommits++
			result.Committed = true
			result.Status = "partially_committed"
		}
	}
	if strings.TrimSpace(request.Verification) != "" {
		op, err := parseMongoOperation(request.Verification)
		if err != nil || mongoOpKinds[op.Op] != "read" {
			return result, errors.New("dosql_verification_invalid: verification must be one find, count or aggregate")
		}
		verified, err := runDoSQLMongoOperation(runCtx, database, op, request.Verification)
		if err != nil {
			return result, failure("dosql_verification_failed", err)
		}
		result.Verification = &verified
	}
	result.DurationMS = time.Since(started).Milliseconds()
	if request.ReadOnly || request.DryRun {
		result.Status = "verified"
		if request.DryRun {
			result.Status = "dry_run_rolled_back"
		}
		return result, nil
	}
	if transactionOpen {
		if err := session.CommitTransaction(ctx); err != nil {
			return result, failure("dosql_commit_failed", err)
		}
		transactionOpen = false
	}
	result.Committed = true
	result.Status = "verified"
	return result, nil
}

func runDoSQLMongoOperation(ctx context.Context, database *mongo.Database, op mongoOperation, statement string) (doSQLStatementResult, error) {
	executed := doSQLStatementResult{Statement: redactSecretText(statement)}
	collection := database.Collection(op.Collection)
	switch op.Op {
	case "find":
		filter, err := mongoBSON(op.Filter)
		if err != nil {
			return executed, err
		}
		limit := op.Limit
		if limit == 0 || limit > doSQLDirectRowLimit {
			limit = doSQLDirectRowLimit
		}
		find := options.Find().SetLimit(limit + 1)
		if len(op.Sort) != 0 {
			sort, err := mongoBSON(op.Sort)
			if err != nil {
				return executed, err
			}
			find.SetSort(sort)
		}
		if len(op.Projection) != 0 {
			projection, err := mongoBSON(op.Projection)
			if err != nil {
				return executed, err
			}
			find.SetProjection(projection)
		}
		cursor, err := collection.Find(ctx, filter, find)
		if err != nil {
			return executed, err
		}
		return collectMongoDocuments(ctx, cursor, executed, int(limit))
	case "aggregate":
		var stages []json.RawMessage
		if err := json.Unmarshal(op.Pipeline, &stages); err != nil {
			return executed, err
		}
		pipeline := make([]bson.D, 0, len(stages))
		for _, stage := range stages {
			doc, err := mongoBSON(stage)
			if err != nil {
				return executed, err
			}
			pipeline = append(pipeline, doc)
		}
		cursor, err := collection.Aggregate(ctx, pipeline)
		if err != nil {
			return executed, err
		}
		return collectMongoDocuments(ctx, cursor, executed, doSQLDirectRowLimit)
	case "count":
		filter, err := mongoBSON(op.Filter)
		if err != nil {
			return executed, err
		}
		count, err := collection.CountDocuments(ctx, filter)
		if err != nil {
			return executed, err
		}
		executed.Columns, executed.Rows = []string{"count"}, [][]string{{fmt.Sprint(count)}}
	case "insertOne":
		document, err := mongoBSON(op.Document)
		if err != nil {
			return executed, err
		}
		if _, err := collection.InsertOne(ctx, document); err != nil {
			return executed, err
		}
		executed.AffectedRows = 1
	case "insertMany":
		documents, err := mongoBSONList(op.Documents)
		if err != nil {
			return executed, err
		}
		items := make([]interface{}, len(documents))
		for index := range documents {
			items[index] = documents[index]
		}
		inserted, err := collection.InsertMany(ctx, items)
		if err != nil {
			return executed, err
		}
		executed.AffectedRows = int64(len(inserted.InsertedIDs))
	case "updateOne", "updateMany":
		filter, err := mongoBSON(op.Filter)
		if err != nil {
			return executed, err
		}
		var update interface{}
		if strings.HasPrefix(strings.TrimSpace(string(op.Update)), "[") {
			stages, err := mongoBSONList(op.Update)
			if err != nil {
				return executed, err
			}
			update = stages
		} else if update, err = mongoBSON(op.Update); err != nil {
			return executed, err
		}
		var outcome *mongo.UpdateResult
		if op.Op == "updateOne" {
			outcome, err = collection.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(op.Upsert))
		} else {
			outcome, err = collection.UpdateMany(ctx, filter, update, options.UpdateMany().SetUpsert(op.Upsert))
		}
		if err != nil {
			return executed, err
		}
		executed.AffectedRows = outcome.ModifiedCount + outcome.UpsertedCount
	case "replaceOne":
		filter, err := mongoBSON(op.Filter)
		if err != nil {
			return executed, err
		}
		replacement, err := mongoBSON(op.Replacement)
		if err != nil {
			return executed, err
		}
		outcome, err := collection.ReplaceOne(ctx, filter, replacement, options.Replace().SetUpsert(op.Upsert))
		if err != nil {
			return executed, err
		}
		executed.AffectedRows = outcome.ModifiedCount + outcome.UpsertedCount
	case "deleteOne", "deleteMany":
		filter, err := mongoBSON(op.Filter)
		if err != nil {
			return executed, err
		}
		var outcome *mongo.DeleteResult
		if op.Op == "deleteOne" {
			outcome, err = collection.DeleteOne(ctx, filter)
		} else {
			outcome, err = collection.DeleteMany(ctx, filter)
		}
		if err != nil {
			return executed, err
		}
		executed.AffectedRows = outcome.DeletedCount
	case "createCollection":
		if err := database.CreateCollection(ctx, op.Collection); err != nil {
			return executed, err
		}
	case "createIndex":
		keys, err := mongoBSON(op.Keys)
		if err != nil {
			return executed, err
		}
		index := options.Index().SetUnique(op.Unique)
		if op.Name != "" {
			index.SetName(op.Name)
		}
		if op.ExpireAfterSeconds != nil {
			index.SetExpireAfterSeconds(*op.ExpireAfterSeconds)
		}
		if op.Sparse {
			index.SetSparse(true)
		}
		if len(op.PartialFilter) != 0 {
			partial, err := mongoBSON(op.PartialFilter)
			if err != nil {
				return executed, err
			}
			index.SetPartialFilterExpression(partial)
		}
		if _, err := collection.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: keys, Options: index}); err != nil {
			return executed, err
		}
	case "dropIndex":
		if err := collection.Indexes().DropOne(ctx, op.Name); err != nil {
			return executed, err
		}
	case "drop":
		if err := collection.Drop(ctx); err != nil {
			return executed, err
		}
	default:
		return runDoSQLMongoExtended(ctx, database, op, executed)
	}
	return executed, nil
}

func collectMongoDocuments(ctx context.Context, cursor *mongo.Cursor, executed doSQLStatementResult, limit int) (doSQLStatementResult, error) {
	defer cursor.Close(ctx)
	executed.Columns = []string{"document"}
	for cursor.Next(ctx) {
		if len(executed.Rows) == limit {
			executed.Truncated = true
			break
		}
		var document bson.D
		if err := cursor.Decode(&document); err != nil {
			return executed, err
		}
		executed.Rows = append(executed.Rows, []string{mongoDocumentText(document)})
	}
	return executed, cursor.Err()
}
