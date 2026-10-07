//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func stickyGateTestAccounts() map[int64]*Account {
	past := time.Now().Add(-time.Hour)
	mk := func(id int64, proxy *Proxy) *Account {
		pid := int64(9000 + id)
		if proxy != nil {
			proxy.ID = pid
		}
		return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, ProxyID: &pid, Proxy: proxy}
	}
	return map[int64]*Account{
		1: mk(1, &Proxy{Status: StatusActive}),
		2: mk(2, nil),
		3: mk(3, &Proxy{Status: "inactive"}),
		4: mk(4, &Proxy{Status: StatusActive, ExpiresAt: &past}),
		5: {ID: 5, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true},
	}
}

func TestStickyAccountFetchRejectsUnavailableProxy(t *testing.T) {
	repo := &tokenRefreshAccountRepo{}
	repo.accountsByID = stickyGateTestAccounts()
	fetchers := map[string]func(int64) (*Account, error){
		"gateway": func(id int64) (*Account, error) {
			return (&GatewayService{accountRepo: repo}).getSchedulableAccount(context.Background(), id)
		},
		"openai": func(id int64) (*Account, error) {
			return (&OpenAIGatewayService{accountRepo: repo}).getSchedulableAccount(context.Background(), id)
		},
		"gemini": func(id int64) (*Account, error) {
			return (&GeminiMessagesCompatService{accountRepo: repo}).getSchedulableAccount(context.Background(), id)
		},
	}
	for name, fetch := range fetchers {
		t.Run(name, func(t *testing.T) {
			for _, id := range []int64{1, 5} {
				got, err := fetch(id)
				require.NoError(t, err)
				require.NotNil(t, got, "account %d has a usable proxy or none and must stay selectable", id)
			}
			for _, id := range []int64{2, 3, 4} {
				got, err := fetch(id)
				require.NoError(t, err)
				require.Nil(t, got, "account %d has a deleted/inactive/expired proxy and must not be returned for sticky selection", id)
			}
		})
	}
}
