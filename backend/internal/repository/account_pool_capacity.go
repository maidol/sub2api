package repository

import (
	"context"
	"database/sql"
	"errors"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// guardPoolProxyCapacity enforces a pool-managed proxy's account limit. It
// must run in the transaction that writes the account: it locks the proxy row
// so the count and the write cannot interleave with another save.
//
// Shadows follow their parent's proxy and take no place; an account already
// on the proxy keeps it even when the limit has since been lowered.
func guardPoolProxyCapacity(ctx context.Context, client *dbent.Client, account *service.Account) error {
	if account == nil || account.ProxyID == nil || account.IsShadow() {
		return nil
	}
	proxyID := *account.ProxyID

	if account.ID > 0 {
		var current sql.NullInt64
		err := scanSingleRow(ctx, client, "SELECT proxy_id FROM accounts WHERE id = $1", []any{account.ID}, &current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && current.Valid && current.Int64 == proxyID {
			return nil
		}
	}

	var managedBy string
	var shareable bool
	var shareMax int
	err := scanSingleRow(ctx, client,
		"SELECT managed_by, pool_shareable, pool_share_max FROM proxies WHERE id = $1 AND deleted_at IS NULL FOR UPDATE",
		[]any{proxyID}, &managedBy, &shareable, &shareMax)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrProxyNotFound
	}
	if err != nil {
		return err
	}
	if managedBy != service.ProxyManagedByPool {
		return nil
	}

	var used int64
	if err := scanSingleRow(ctx, client,
		"SELECT COUNT(*) FROM accounts WHERE proxy_id = $1 AND id <> $2 AND deleted_at IS NULL AND parent_account_id IS NULL",
		[]any{proxyID, account.ID}, &used); err != nil {
		return err
	}
	p := service.Proxy{PoolShareable: shareable, PoolShareMax: shareMax}
	capacity, limited := p.PoolCapacity()
	switch {
	case !limited || used < capacity:
		return nil
	case !shareable:
		return service.ErrProxyPoolSlotNotShared
	default:
		return service.NewProxyPoolSlotFullError(used, capacity)
	}
}
