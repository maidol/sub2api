//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
)

func TestUpdateProxySavesShareSettingsOnPoolProxiesOnly(t *testing.T) {
	repo := newPoolProxyRepo()
	repo.rows[1] = &Proxy{ID: 1, Name: "slot01", ManagedBy: ProxyManagedByPool, FallbackMode: FallbackModeNone}
	repo.rows[2] = &Proxy{ID: 2, Name: "hand-made", FallbackMode: FallbackModeNone}
	svc := &adminServiceImpl{proxyRepo: repo}
	yes, five := true, 5

	got, err := svc.UpdateProxy(context.Background(), 1, &UpdateProxyInput{PoolShareable: &yes, PoolShareMax: &five})
	if err != nil || !got.PoolShareable || got.PoolShareMax != 5 {
		t.Fatalf("pool proxy: %+v, %v", got, err)
	}
	got, err = svc.UpdateProxy(context.Background(), 2, &UpdateProxyInput{PoolShareable: &yes, PoolShareMax: &five})
	if err != nil || got.PoolShareable || got.PoolShareMax != 0 {
		t.Fatalf("hand-made proxy must ignore share settings: %+v, %v", got, err)
	}
	neg := -1
	if _, err := svc.UpdateProxy(context.Background(), 1, &UpdateProxyInput{PoolShareMax: &neg}); !errors.Is(err, ErrProxyPoolShareMaxInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestBulkUpdateRejectsAPoolProxy(t *testing.T) {
	repo := newPoolProxyRepo()
	repo.rows[1] = &Proxy{ID: 1, ManagedBy: ProxyManagedByPool}
	svc := &adminServiceImpl{proxyRepo: repo}
	id := int64(1)
	_, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{10}, ProxyID: &id})
	if !errors.Is(err, ErrProxyPoolBulkUnsupported) {
		t.Fatalf("err = %v", err)
	}
}
