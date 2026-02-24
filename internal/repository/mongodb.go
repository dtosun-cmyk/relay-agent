package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"relay-agent/internal/util"
)

// MongoRepository implements the repository interface using MongoDB
type MongoRepository struct {
	client   *mongo.Client
	database *mongo.Database
	emails   *mongo.Collection
	stats    *mongo.Collection // hourly_stats collection

	logger zerolog.Logger
}

// NewMongoRepository creates a new MongoDB repository with connection pooling
func NewMongoRepository(uri, dbName string, logger zerolog.Logger) (*MongoRepository, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Configure client options with connection pool settings
	clientOpts := options.Client().
		ApplyURI(uri).
		SetMaxPoolSize(100). // Maximum connections in pool
		SetMinPoolSize(10).  // Minimum connections to maintain
		SetMaxConnIdleTime(30 * time.Second).
		SetServerSelectionTimeout(5 * time.Second).
		SetConnectTimeout(10 * time.Second)

	client, err := mongo.Connect(ctx, clientOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}

	// Ping to verify connection
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to ping MongoDB: %w", err)
	}

	database := client.Database(dbName)

	repo := &MongoRepository{
		client:   client,
		database: database,
		emails:   database.Collection("emails"),
		stats:    database.Collection("hourly_stats"),
		logger:   logger,
	}

	logger.Info().
		Str("database", dbName).
		Msg("MongoDB repository initialized")

	return repo, nil
}

// EnsureIndexes creates all necessary indexes for optimal query performance
func (r *MongoRepository) EnsureIndexes(ctx context.Context) error {
	indexes := []mongo.IndexModel{
		// Unique sparse index on queue_id for fast lookups
		// Sparse allows multiple documents with missing/null queue_id without duplicate key errors
		{
			Keys:    bson.D{{Key: "queue_id", Value: 1}},
			Options: options.Index().SetUnique(true).SetSparse(true).SetName("idx_queue_id"),
		},
		// Index on mailgateway_queue_id for webhook correlation
		{
			Keys:    bson.D{{Key: "mailgateway_queue_id", Value: 1}},
			Options: options.Index().SetName("idx_mailgateway_queue_id").SetSparse(true),
		},
		// Compound index for provider stats queries
		{
			Keys: bson.D{
				{Key: "recipient_domain", Value: 1},
				{Key: "created_at", Value: -1},
			},
			Options: options.Index().SetName("idx_recipient_domain_created"),
		},
		// Compound index for log queries by status
		{
			Keys: bson.D{
				{Key: "status", Value: 1},
				{Key: "created_at", Value: -1},
			},
			Options: options.Index().SetName("idx_status_created"),
		},
		// Index on created_at for time-based queries and sorting
		{
			Keys:    bson.D{{Key: "created_at", Value: -1}},
			Options: options.Index().SetName("idx_created_at"),
		},
		// TTL index for automatic cleanup after 30 days (optional)
		{
			Keys: bson.D{{Key: "created_at", Value: 1}},
			Options: options.Index().
				SetName("idx_created_at_ttl").
				SetExpireAfterSeconds(30 * 24 * 60 * 60), // 30 days
		},
		// Compound index for pending webhook queries
		{
			Keys: bson.D{
				{Key: "webhook_sent", Value: 1},
				{Key: "status", Value: 1},
			},
			Options: options.Index().SetName("idx_webhook_status"),
		},
		// Index on sender for filtering
		{
			Keys:    bson.D{{Key: "sender", Value: 1}},
			Options: options.Index().SetName("idx_sender"),
		},
		// Index on recipient for filtering
		{
			Keys:    bson.D{{Key: "recipient", Value: 1}},
			Options: options.Index().SetName("idx_recipient"),
		},
	}

	_, err := r.emails.Indexes().CreateMany(ctx, indexes)
	if err != nil {
		return fmt.Errorf("failed to create email indexes: %w", err)
	}

	// Create indexes for hourly_stats collection
	statsIndexes := []mongo.IndexModel{
		// Unique compound index on date and hour
		{
			Keys: bson.D{
				{Key: "date", Value: 1},
				{Key: "hour", Value: 1},
			},
			Options: options.Index().SetUnique(true).SetName("idx_date_hour"),
		},
		// Index on date for daily aggregations
		{
			Keys:    bson.D{{Key: "date", Value: -1}},
			Options: options.Index().SetName("idx_date"),
		},
	}

	_, err = r.stats.Indexes().CreateMany(ctx, statsIndexes)
	if err != nil {
		return fmt.Errorf("failed to create stats indexes: %w", err)
	}

	r.logger.Info().Msg("All indexes created successfully")
	return nil
}

// InsertBatch performs a batch insert with ordered:false for maximum performance
func (r *MongoRepository) InsertBatch(ctx context.Context, emails []*Email) error {
	if len(emails) == 0 {
		return nil
	}

	// Convert []*Email to []interface{} for InsertMany
	docs := make([]interface{}, len(emails))
	for i, email := range emails {
		docs[i] = email
	}

	// Use ordered:false to continue on duplicate key errors
	opts := options.InsertMany().SetOrdered(false)

	result, err := r.emails.InsertMany(ctx, docs, opts)
	if err != nil {
		// Check if it's a bulk write error (partial failure)
		if bulkErr, ok := err.(mongo.BulkWriteException); ok {
			inserted := len(result.InsertedIDs)
			failed := len(bulkErr.WriteErrors)

			r.logger.Warn().
				Int("inserted", inserted).
				Int("failed", failed).
				Msg("Batch insert completed with some failures (likely duplicates)")

			// If some documents were inserted, consider it a partial success
			if inserted > 0 {
				return nil
			}
		}
		return fmt.Errorf("failed to insert batch: %w", err)
	}

	r.logger.Debug().
		Int("count", len(result.InsertedIDs)).
		Msg("Batch inserted emails")

	return nil
}

// UpsertByMailgatewayQueueID updates an existing email record by MailgatewayQueueID
// or inserts a new one if it doesn't exist. This prevents duplicate records when
// the SMTP filter creates a "received" record and the log parser creates a "sent/bounced" record.
// When updating, it preserves the original ReceivedAt timestamp from the filter.
func (r *MongoRepository) UpsertByMailgatewayQueueID(ctx context.Context, email *Email) error {
	if email.MailgatewayQueueID == "" {
		return fmt.Errorf("mailgateway_queue_id is required for upsert")
	}

	filter := bson.M{"mailgateway_queue_id": email.MailgatewayQueueID}

	// Build update document:
	// - $set: Delivery-related fields from log parser (relay info, status, etc.)
	// - $setOnInsert: Original client info from filter (mailgateway source)
	update := bson.M{
		"$set": bson.M{
			"queue_id":         email.QueueID,
			"status":           email.Status,
			"dsn":              email.DSN,
			"status_message":   email.StatusMessage,
			"delivery_time_ms": email.DeliveryTimeMs,
			"delivered_at":     email.DeliveredAt,
			"provider":         email.Provider,
			"attempts":         email.Attempts,
			"created_at":       email.CreatedAt,
			"relay_host":       email.RelayHost, // Destination relay (from log parser)
			"relay_ip":         email.RelayIP,   // Destination relay IP (from log parser)
		},
		"$setOnInsert": bson.M{
			// These fields are only set if this is a new insert (not an update)
			// This preserves original mailgateway info (from XFORWARD) and ReceivedAt
			"sender":               email.Sender,
			"recipient":            email.Recipient,
			"recipient_domain":     email.RecipientDomain,
			"size":                 email.Size,
			"received_at":          email.ReceivedAt,
			"client_host":          email.ClientHost, // Mailgateway hostname (source)
			"client_ip":            email.ClientIP,   // Mailgateway IP (source)
			"webhook_sent":         false,
			"mailgateway_queue_id": email.MailgatewayQueueID,
		},
	}

	opts := options.Update().SetUpsert(true)

	result, err := r.emails.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return fmt.Errorf("failed to upsert email by mailgateway_queue_id: %w", err)
	}

	if result.UpsertedCount > 0 {
		r.logger.Debug().
			Str("mailgateway_queue_id", email.MailgatewayQueueID).
			Str("queue_id", email.QueueID).
			Msg("Inserted new email via upsert")
	} else if result.ModifiedCount > 0 {
		r.logger.Debug().
			Str("mailgateway_queue_id", email.MailgatewayQueueID).
			Str("queue_id", email.QueueID).
			Str("status", email.Status).
			Msg("Updated existing email record")
	}

	return nil
}

// BulkUpsertByMailgatewayQueueID performs bulk upsert operations for better performance
// Instead of individual upserts, this batches all operations into a single MongoDB call
func (r *MongoRepository) BulkUpsertByMailgatewayQueueID(ctx context.Context, emails []*Email) error {
	if len(emails) == 0 {
		return nil
	}

	operations := make([]mongo.WriteModel, 0, len(emails))

	for _, email := range emails {
		if email.MailgatewayQueueID == "" {
			continue
		}

		filter := bson.M{"mailgateway_queue_id": email.MailgatewayQueueID}

		update := bson.M{
			"$set": bson.M{
				"queue_id":         email.QueueID,
				"status":           email.Status,
				"dsn":              email.DSN,
				"status_message":   email.StatusMessage,
				"delivery_time_ms": email.DeliveryTimeMs,
				"delivered_at":     email.DeliveredAt,
				"provider":         email.Provider,
				"attempts":         email.Attempts,
				"created_at":       email.CreatedAt,
				"relay_host":       email.RelayHost,
				"relay_ip":         email.RelayIP,
				"updated_at":       util.NowTurkey(),
			},
			"$setOnInsert": bson.M{
				"sender":               email.Sender,
				"recipient":            email.Recipient,
				"recipient_domain":     email.RecipientDomain,
				"size":                 email.Size,
				"received_at":          email.ReceivedAt,
				"client_host":          email.ClientHost,
				"client_ip":            email.ClientIP,
				"webhook_sent":         false,
				"mailgateway_queue_id": email.MailgatewayQueueID,
			},
		}

		operation := mongo.NewUpdateOneModel().
			SetFilter(filter).
			SetUpdate(update).
			SetUpsert(true)

		operations = append(operations, operation)
	}

	if len(operations) == 0 {
		return nil
	}

	opts := options.BulkWrite().SetOrdered(false)
	result, err := r.emails.BulkWrite(ctx, operations, opts)

	if err != nil {
		// Check for partial failures
		if bulkErr, ok := err.(mongo.BulkWriteException); ok {
			r.logger.Warn().
				Int64("upserted", result.UpsertedCount).
				Int64("modified", result.ModifiedCount).
				Int("errors", len(bulkErr.WriteErrors)).
				Msg("Bulk upsert completed with some errors")
			return nil
		}
		return fmt.Errorf("bulk upsert failed: %w", err)
	}

	r.logger.Debug().
		Int64("upserted", result.UpsertedCount).
		Int64("modified", result.ModifiedCount).
		Int("total", len(operations)).
		Msg("Bulk upsert completed")

	return nil
}

// UpdateWebhookStatus marks a webhook as sent for the given queue ID
func (r *MongoRepository) UpdateWebhookStatus(ctx context.Context, queueID string) error {
	filter := bson.M{"queue_id": queueID}
	update := bson.M{
		"$set": bson.M{
			"webhook_sent": true,
			"updated_at":   util.NowTurkey(),
		},
	}

	result, err := r.emails.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("failed to update webhook status: %w", err)
	}

	if result.MatchedCount == 0 {
		return fmt.Errorf("email with queue_id %s not found", queueID)
	}

	r.logger.Debug().
		Str("queue_id", queueID).
		Msg("Updated webhook status")

	return nil
}

// UpdateByRecipientMatch finds a recent "received" record matching recipient+sender
// and updates it with delivery info. Used when mailgateway_queue_id is not available
// (e.g., reinjected emails from content_filter).
// Returns the mailgateway_queue_id if a match was found and updated.
func (r *MongoRepository) UpdateByRecipientMatch(ctx context.Context, email *Email) (string, error) {
	// Find a "received" record with same recipient+sender from last 10 minutes
	// Extended timeout to handle deferred emails that retry later
	cutoff := util.NowTurkey().Add(-10 * time.Minute)

	filter := bson.M{
		"recipient":            email.Recipient,
		"sender":               email.Sender,
		"status":               "received",
		"mailgateway_queue_id": bson.M{"$ne": ""}, // Must have mailgateway_queue_id
		"received_at":          bson.M{"$gte": cutoff},
	}

	update := bson.M{
		"$set": bson.M{
			"queue_id":         email.QueueID,
			"status":           email.Status,
			"dsn":              email.DSN,
			"status_message":   email.StatusMessage,
			"delivery_time_ms": email.DeliveryTimeMs,
			"delivered_at":     email.DeliveredAt,
			"provider":         email.Provider,
			"relay_host":       email.RelayHost,
			"relay_ip":         email.RelayIP,
			"attempts":         email.Attempts,
			"updated_at":       util.NowTurkey(),
		},
	}

	// Use FindOneAndUpdate to get the mailgateway_queue_id
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var updatedEmail Email
	err := r.emails.FindOneAndUpdate(ctx, filter, update, opts).Decode(&updatedEmail)

	if err != nil {
		if err == mongo.ErrNoDocuments {
			return "", nil // No match found
		}
		return "", fmt.Errorf("failed to update by recipient match: %w", err)
	}

	r.logger.Debug().
		Str("recipient", email.Recipient).
		Str("mailgateway_queue_id", updatedEmail.MailgatewayQueueID).
		Str("status", email.Status).
		Str("relay_host", email.RelayHost).
		Msg("Updated filter record with delivery info")

	return updatedEmail.MailgatewayQueueID, nil
}

// UpdateWebhookStatusByMailgatewayID marks a webhook as sent for the given mailgateway queue ID
func (r *MongoRepository) UpdateWebhookStatusByMailgatewayID(ctx context.Context, mailgatewayQueueID string) error {
	filter := bson.M{"mailgateway_queue_id": mailgatewayQueueID}
	update := bson.M{
		"$set": bson.M{
			"webhook_sent": true,
			"updated_at":   util.NowTurkey(),
		},
	}

	result, err := r.emails.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("failed to update webhook status: %w", err)
	}

	if result.MatchedCount == 0 {
		r.logger.Debug().
			Str("mailgateway_queue_id", mailgatewayQueueID).
			Msg("Email not found for webhook status update (may not have been inserted yet)")
		return nil // Not an error - the email may not have been inserted yet
	}

	r.logger.Debug().
		Str("mailgateway_queue_id", mailgatewayQueueID).
		Msg("Updated webhook status")

	return nil
}

// GetLogs retrieves emails based on filter criteria with pagination
func (r *MongoRepository) GetLogs(ctx context.Context, filter LogFilter, limit, offset int) ([]Email, int64, error) {
	// Build MongoDB filter
	mongoFilter := bson.M{}

	if filter.Status != "" {
		mongoFilter["status"] = filter.Status
	}

	if filter.Sender != "" {
		// Use regex for partial matching
		mongoFilter["sender"] = bson.M{"$regex": filter.Sender, "$options": "i"}
	}

	if filter.Recipient != "" {
		mongoFilter["recipient"] = bson.M{"$regex": filter.Recipient, "$options": "i"}
	}

	if filter.Domain != "" {
		mongoFilter["recipient_domain"] = filter.Domain
	}

	if !filter.StartDate.IsZero() || !filter.EndDate.IsZero() {
		dateFilter := bson.M{}
		if !filter.StartDate.IsZero() {
			dateFilter["$gte"] = filter.StartDate
		}
		if !filter.EndDate.IsZero() {
			dateFilter["$lte"] = filter.EndDate
		}
		mongoFilter["created_at"] = dateFilter
	}

	// Count total matching documents (with limit for performance)
	countOpts := options.Count().SetLimit(int64(limit + offset + 1))
	total, err := r.emails.CountDocuments(ctx, mongoFilter, countOpts)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count documents: %w", err)
	}

	// Query options with pagination and sorting
	findOpts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}). // Newest first
		SetSkip(int64(offset)).
		SetLimit(int64(limit))

	// Projection to return only needed fields (optional optimization)
	// Uncomment if you want to reduce data transfer
	// findOpts.SetProjection(bson.M{
	// 	"queue_id":              1,
	// 	"sender":                1,
	// 	"recipient":             1,
	// 	"recipient_domain":      1,
	// 	"status":                1,
	// 	"created_at":            1,
	// 	"mailgateway_queue_id":  1,
	// 	"webhook_sent":          1,
	// })

	cursor, err := r.emails.Find(ctx, mongoFilter, findOpts)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to query logs: %w", err)
	}
	defer cursor.Close(ctx)

	var emails []Email
	if err := cursor.All(ctx, &emails); err != nil {
		return nil, 0, fmt.Errorf("failed to decode logs: %w", err)
	}

	r.logger.Debug().
		Int("count", len(emails)).
		Int64("total", total).
		Interface("filter", filter).
		Msg("Retrieved logs")

	return emails, total, nil
}

// GetTodayStats calculates today's statistics
func (r *MongoRepository) GetTodayStats(ctx context.Context) (*DayStats, error) {
	today := util.NowTurkey().Truncate(24 * time.Hour)
	tomorrow := today.Add(24 * time.Hour)

	filter := bson.M{
		"created_at": bson.M{
			"$gte": today,
			"$lt":  tomorrow,
		},
	}

	// Aggregate to get status counts
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: filter}},
		{{Key: "$group", Value: bson.M{
			"_id":   "$status",
			"count": bson.M{"$sum": 1},
		}}},
	}

	cursor, err := r.emails.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate today's stats: %w", err)
	}
	defer cursor.Close(ctx)

	stats := &DayStats{
		Date: today.Format("2006-01-02"),
	}

	type statusCount struct {
		Status string `bson:"_id"`
		Count  int    `bson:"count"`
	}

	for cursor.Next(ctx) {
		var sc statusCount
		if err := cursor.Decode(&sc); err != nil {
			return nil, fmt.Errorf("failed to decode status count: %w", err)
		}

		stats.Total += sc.Count

		switch sc.Status {
		case "sent":
			stats.Sent = sc.Count
		case "deferred":
			stats.Deferred = sc.Count
		case "bounced":
			stats.Bounced = sc.Count
		}
	}

	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("cursor error: %w", err)
	}

	r.logger.Debug().
		Interface("stats", stats).
		Msg("Retrieved today's stats")

	return stats, nil
}

// GetHourlyStats retrieves hourly statistics for a specific date
func (r *MongoRepository) GetHourlyStats(ctx context.Context, date string) ([]HourlyStats, error) {
	filter := bson.M{"date": date}
	opts := options.Find().SetSort(bson.D{{Key: "hour", Value: 1}})

	cursor, err := r.stats.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to query hourly stats: %w", err)
	}
	defer cursor.Close(ctx)

	var stats []HourlyStats
	if err := cursor.All(ctx, &stats); err != nil {
		return nil, fmt.Errorf("failed to decode hourly stats: %w", err)
	}

	r.logger.Debug().
		Str("date", date).
		Int("count", len(stats)).
		Msg("Retrieved hourly stats")

	return stats, nil
}

// SaveHourlyStats saves or updates hourly statistics using upsert
func (r *MongoRepository) SaveHourlyStats(ctx context.Context, stats *HourlyStats) error {
	filter := bson.M{
		"date": stats.Date,
		"hour": stats.Hour,
	}

	update := bson.M{
		"$set": bson.M{
			"sent":       stats.Sent,
			"deferred":   stats.Deferred,
			"bounced":    stats.Bounced,
			"total":      stats.Total,
			"updated_at": util.NowTurkey(),
		},
	}

	opts := options.Update().SetUpsert(true)

	_, err := r.stats.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return fmt.Errorf("failed to save hourly stats: %w", err)
	}

	r.logger.Debug().
		Time("date", stats.Date).
		Int("hour", stats.Hour).
		Msg("Saved hourly stats")

	return nil
}

// GetQueueStats returns current queue statistics from Postfix queue status
func (r *MongoRepository) GetQueueStats(ctx context.Context) (active, deferred, hold, total int, err error) {
	now := util.NowTurkey()
	today := now.Truncate(24 * time.Hour)

	// Count active emails (sent today)
	activeFilter := bson.M{
		"status": "sent",
		"created_at": bson.M{
			"$gte": today,
		},
	}
	activeCount, err := r.emails.CountDocuments(ctx, activeFilter)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("failed to count active: %w", err)
	}
	active = int(activeCount)

	// Count deferred emails
	deferredFilter := bson.M{"status": "deferred"}
	deferredCount, err := r.emails.CountDocuments(ctx, deferredFilter)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("failed to count deferred: %w", err)
	}
	deferred = int(deferredCount)

	// Count held emails (bounced or on hold)
	holdFilter := bson.M{
		"status": bson.M{
			"$in": []string{"bounced", "hold"},
		},
	}
	holdCount, err := r.emails.CountDocuments(ctx, holdFilter)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("failed to count hold: %w", err)
	}
	hold = int(holdCount)

	// Total count for today
	totalFilter := bson.M{
		"created_at": bson.M{
			"$gte": today,
		},
	}
	totalCount, err := r.emails.CountDocuments(ctx, totalFilter)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("failed to count total: %w", err)
	}
	total = int(totalCount)

	r.logger.Debug().
		Int("active", active).
		Int("deferred", deferred).
		Int("hold", hold).
		Int("total", total).
		Msg("Retrieved queue stats")

	return active, deferred, hold, total, nil
}

// Ping verifies the connection to MongoDB
func (r *MongoRepository) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	if err := r.client.Ping(ctx, nil); err != nil {
		return fmt.Errorf("failed to ping MongoDB: %w", err)
	}

	return nil
}

// Close gracefully closes the MongoDB connection
func (r *MongoRepository) Close(ctx context.Context) error {
	if err := r.client.Disconnect(ctx); err != nil {
		return fmt.Errorf("failed to disconnect from MongoDB: %w", err)
	}

	r.logger.Info().Msg("MongoDB connection closed")
	return nil
}
