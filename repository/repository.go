package repository

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type DBConnection interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
}

type Repository struct {
	conn DBConnection
}

func NewRepository(conn DBConnection) *Repository {
	return &Repository{conn}
}

func (r *Repository) LoadConfigs(ctx context.Context, lastID int64) ([]Config, error) {
	rows, err := r.conn.Query(ctx, "SELECT id, user_id, secret, cipher FROM config WHERE id > $1", lastID)
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var configs []Config
	for rows.Next() {
		var config Config
		if err := rows.Scan(&config.ID, &config.UserID, &config.Secret, &config.Cipher); err != nil {
			return nil, err
		}

		configs = append(configs, config)
	}

	return configs, nil
}
