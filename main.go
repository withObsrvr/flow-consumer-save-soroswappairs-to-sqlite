package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/withObsrvr/pluginapi"
)

// SaveSoroswapPairsToSQLite implements the pluginapi.Consumer interface
type SaveSoroswapPairsToSQLite struct {
	db      *sql.DB
	dbPath  string
	name    string
	version string
}

// Event types
type NewPairEvent struct {
	Type        string    `json:"type"`
	PairAddress string    `json:"pair_address"`
	Token0      string    `json:"token_0"`
	Token1      string    `json:"token_1"`
	Timestamp   time.Time `json:"timestamp"`
}

type SyncEvent struct {
	Type           string    `json:"type"`
	ContractID     string    `json:"contract_id"`
	NewReserve0    string    `json:"new_reserve_0"`
	NewReserve1    string    `json:"new_reserve_1"`
	Timestamp      time.Time `json:"timestamp"`
	LedgerSequence int64     `json:"ledger_sequence"`
}

// New creates a new instance of the plugin
func New() pluginapi.Plugin {
	return &SaveSoroswapPairsToSQLite{
		name:    "SaveSoroswapPairsToSQLite",
		version: "1.0.0",
	}
}

// Name returns the name of the plugin
func (s *SaveSoroswapPairsToSQLite) Name() string {
	return s.name
}

// Version returns the version of the plugin
func (s *SaveSoroswapPairsToSQLite) Version() string {
	return s.version
}

// Type returns the type of the plugin
func (s *SaveSoroswapPairsToSQLite) Type() pluginapi.PluginType {
	return pluginapi.ConsumerPlugin
}

// Initialize sets up the SQLite database
func (s *SaveSoroswapPairsToSQLite) Initialize(config map[string]interface{}) error {
	dbPath, ok := config["db_path"].(string)
	if !ok {
		dbPath = "soroswap_pairs.sqlite"
	}
	s.dbPath = dbPath

	// Open SQLite connection
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open SQLite: %v", err)
	}

	// Verify connection
	if err := db.Ping(); err != nil {
		return fmt.Errorf("failed to ping SQLite: %v", err)
	}

	// Set pragmas for better performance
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;"); err != nil {
		return fmt.Errorf("failed to set SQLite pragmas: %v", err)
	}

	// Create table with proper constraints
	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS soroswap_pairs (
            pair_address TEXT NOT NULL PRIMARY KEY,
            token_0 TEXT NOT NULL,
            token_1 TEXT NOT NULL,
            reserve_0 TEXT NOT NULL DEFAULT '0',
            reserve_1 TEXT NOT NULL DEFAULT '0',
            created_at TIMESTAMP NOT NULL,
            last_sync_at TIMESTAMP,
            last_sync_ledger INTEGER,
            
            -- Add constraints to prevent empty strings
            CHECK (length(pair_address) > 0),
            CHECK (length(token_0) > 0),
            CHECK (length(token_1) > 0)
        );

        -- Add an index for faster token lookups
        CREATE INDEX IF NOT EXISTS idx_tokens ON soroswap_pairs(token_0, token_1);
    `)
	if err != nil {
		return fmt.Errorf("failed to create soroswap_pairs table: %v", err)
	}

	s.db = db
	log.Printf("SQLite database initialized at %s", dbPath)
	return nil
}

// Process handles incoming messages
func (s *SaveSoroswapPairsToSQLite) Process(ctx context.Context, msg pluginapi.Message) error {
	// Add timeout to context
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	jsonBytes, ok := msg.Payload.([]byte)
	if !ok {
		log.Printf("Error: expected []byte, got %T", msg.Payload)
		return fmt.Errorf("expected []byte, got %T", msg.Payload)
	}

	// First unmarshal into a temporary struct to check the type
	var temp struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(jsonBytes, &temp); err != nil {
		return fmt.Errorf("error decoding event type: %w", err)
	}

	log.Printf("Processing event type: %s", temp.Type)

	switch temp.Type {
	case "new_pair":
		var newPairEvent NewPairEvent
		if err := json.Unmarshal(jsonBytes, &newPairEvent); err != nil {
			return fmt.Errorf("error decoding new pair event: %w", err)
		}
		return s.handleNewPair(ctx, newPairEvent)

	case "sync":
		var syncEvent SyncEvent
		if err := json.Unmarshal(jsonBytes, &syncEvent); err != nil {
			return fmt.Errorf("error decoding sync event: %w", err)
		}
		return s.handleSync(ctx, syncEvent)

	default:
		return fmt.Errorf("unknown event type: %s", temp.Type)
	}
}

func (s *SaveSoroswapPairsToSQLite) handleNewPair(ctx context.Context, event NewPairEvent) error {
	// Validate input data
	if event.PairAddress == "" || event.Token0 == "" || event.Token1 == "" {
		return fmt.Errorf("invalid new pair event data: missing required fields")
	}

	log.Printf("Attempting to insert new Soroswap pair: %s (tokens: %s/%s)",
		event.PairAddress, event.Token0, event.Token1)

	// Begin transaction for better error handling
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback() // Will be ignored if transaction is committed

	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO soroswap_pairs (
            pair_address, token_0, token_1, created_at,
            reserve_0, reserve_1
        ) VALUES (?, ?, ?, ?, '0', '0')
        ON CONFLICT (pair_address) DO NOTHING
    `)
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %v", err)
	}
	defer stmt.Close()

	result, err := stmt.ExecContext(ctx,
		event.PairAddress,
		event.Token0,
		event.Token1,
		event.Timestamp,
	)
	if err != nil {
		return fmt.Errorf("failed to insert pair: %v", err)
	}

	affectedRows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %v", err)
	}

	log.Printf("Inserted new Soroswap pair: %s (rows affected: %d)", event.PairAddress, affectedRows)

	return tx.Commit()
}

func (s *SaveSoroswapPairsToSQLite) handleSync(ctx context.Context, event SyncEvent) error {
	log.Printf("Checking existence of pair: %s", event.ContractID)

	// Begin transaction
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback() // Will be ignored if transaction is committed

	// First check if the pair exists
	var exists bool
	query := `SELECT EXISTS (
		SELECT 1 FROM soroswap_pairs WHERE pair_address = ?
	)`

	err = tx.QueryRowContext(ctx, query, event.ContractID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check pair existence: %v", err)
	}

	if !exists {
		log.Printf("Warning: Received sync event for unknown pair: %s", event.ContractID)
		return nil
	}

	stmt, err := tx.PrepareContext(ctx, `
        UPDATE soroswap_pairs 
        SET reserve_0 = ?,
            reserve_1 = ?,
            last_sync_at = ?,
            last_sync_ledger = ?
        WHERE pair_address = ?
    `)
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %v", err)
	}
	defer stmt.Close()

	result, err := stmt.ExecContext(ctx,
		event.NewReserve0,
		event.NewReserve1,
		event.Timestamp,
		event.LedgerSequence,
		event.ContractID,
	)
	if err != nil {
		return fmt.Errorf("failed to update pair reserves: %v", err)
	}

	affectedRows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %v", err)
	}

	log.Printf("Updated Soroswap pair reserves: %s (rows affected: %d)", event.ContractID, affectedRows)

	return tx.Commit()
}

// Close closes the database connection
func (s *SaveSoroswapPairsToSQLite) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
