package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestConcurrencyStartupPreservesLiveRequests(t *testing.T) {
	for _, markerExists := range []bool{false, true} {
		for _, initializationFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("legacy_marker=%t/later_initialization_fails=%t", markerExists, initializationFails), func(t *testing.T) {
				server := miniredis.RunT(t)
				blueClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
				greenClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
				t.Cleanup(func() { _ = blueClient.Close() })
				t.Cleanup(func() { _ = greenClient.Close() })
				blue := NewConcurrencyCache(blueClient, 15, 900)
				green := NewConcurrencyCache(greenClient, 15, 900)
				ctx := context.Background()
				if markerExists {
					require.NoError(t, blueClient.Set(ctx, legacyWaitSweepMarkerKey, "1", 0).Err())
				}

				acquired, err := blue.AcquireAccountSlot(ctx, 10, 1, "blue-account-request")
				require.NoError(t, err)
				require.True(t, acquired)
				acquired, err = blue.AcquireUserSlot(ctx, 20, 1, "blue-user-request")
				require.NoError(t, err)
				require.True(t, acquired)
				waiting, err := blue.IncrementAccountWaitCount(ctx, 10, 1)
				require.NoError(t, err)
				require.True(t, waiting)
				waiting, err = blue.IncrementWaitCount(ctx, 20, 1)
				require.NoError(t, err)
				require.True(t, waiting)

				// These legacy counters have no active-index member. Starting a new
				// process must not run a one-off scan that deletes another process's waiters.
				legacyAccountWait := accountWaitKey(30)
				legacyUserWait := waitQueueKey(40)
				require.NoError(t, blueClient.Set(ctx, legacyAccountWait, 2, time.Minute).Err())
				require.NoError(t, blueClient.Set(ctx, legacyUserWait, 3, time.Minute).Err())

				// Exercise the real startup cache operation, then simulate a later
				// initialization error. A failed candidate must leave Blue's state usable.
				laterFailure := errors.New("later application initialization failed")
				initialize := func() error {
					if err := green.CleanupStaleProcessSlots(ctx, "green"); err != nil {
						return err
					}
					if initializationFails {
						return laterFailure
					}
					return nil
				}
				if initializationFails {
					require.ErrorIs(t, initialize(), laterFailure)
				} else {
					require.NoError(t, initialize())
				}

				accountCount, err := blue.GetAccountConcurrency(ctx, 10)
				require.NoError(t, err)
				require.Equal(t, 1, accountCount, "Blue's live request must still occupy its account slot")
				userCount, err := blue.GetUserConcurrency(ctx, 20)
				require.NoError(t, err)
				require.Equal(t, 1, userCount, "Blue's live request must still occupy its user slot")
				for key, want := range map[string]int{accountWaitKey(10): 1, waitQueueKey(20): 1, legacyAccountWait: 2, legacyUserWait: 3} {
					count, err := blueClient.Get(ctx, key).Int()
					require.NoError(t, err, "wait counter %s must survive candidate startup", key)
					require.Equal(t, want, count)
				}
				markerCount, err := blueClient.Exists(ctx, legacyWaitSweepMarkerKey).Result()
				require.NoError(t, err)
				require.Equal(t, markerExists, markerCount == 1, "startup must not perform the legacy wait migration")

				acquired, err = green.AcquireAccountSlot(ctx, 10, 1, "green-account-request")
				require.NoError(t, err)
				require.False(t, acquired, "candidate must not bypass the shared account limit")
				acquired, err = green.AcquireUserSlot(ctx, 20, 1, "green-user-request")
				require.NoError(t, err)
				require.False(t, acquired, "candidate must not bypass the shared user limit")

				// Old requests can finish normally, and only their release opens capacity.
				require.NoError(t, blue.ReleaseAccountSlot(ctx, 10, "blue-account-request"))
				require.NoError(t, blue.ReleaseUserSlot(ctx, 20, "blue-user-request"))
				acquired, err = green.AcquireAccountSlot(ctx, 10, 1, "green-account-request")
				require.NoError(t, err)
				require.True(t, acquired)
				acquired, err = green.AcquireUserSlot(ctx, 20, 1, "green-user-request")
				require.NoError(t, err)
				require.True(t, acquired)
			})
		}
	}
}

func TestConcurrencyStartupReapsOnlyTTLExpiredOrphans(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache, ok := NewConcurrencyCache(client, 15, 900).(*concurrencyCache)
	require.True(t, ok)
	ctx := context.Background()
	now, err := cache.redisUnixSeconds(ctx)
	require.NoError(t, err)

	for _, spec := range []slotIndexSpec{accountSlotIndex, userSlotIndex} {
		t.Run(spec.indexKey, func(t *testing.T) {
			for _, id := range []int64{101, 102, 103} {
				require.NoError(t, client.ZAdd(ctx, spec.slotKey(id), redis.Z{
					Score: float64(now - int64(cache.slotTTLSeconds)), Member: "green-expired-at-ttl",
				}).Err())
				require.NoError(t, client.ZAdd(ctx, spec.indexKey, redis.Z{
					Score: float64(now - 1), Member: strconv.FormatInt(id, 10),
				}).Err())
			}
			// An expired index is not evidence that every request or waiter died.
			require.NoError(t, client.ZAdd(ctx, spec.slotKey(101), redis.Z{
				Score: float64(now), Member: "blue-live-request",
			}).Err())
			require.NoError(t, client.Set(ctx, spec.waitKey(102), 2, time.Minute).Err())
			require.NoError(t, cache.CleanupStaleProcessSlots(ctx, "green"))

			members, err := client.ZRange(ctx, spec.slotKey(101), 0, -1).Result()
			require.NoError(t, err)
			require.Equal(t, []string{"blue-live-request"}, members, "expiry, not owner prefix, determines removal")
			for _, id := range []int64{102, 103} {
				exists, err := client.Exists(ctx, spec.slotKey(id)).Result()
				require.NoError(t, err)
				require.Zero(t, exists, "fully expired slot sets must be removed")
			}
			waiters, err := client.Get(ctx, spec.waitKey(102)).Int()
			require.NoError(t, err)
			require.Equal(t, 2, waiters)
			for _, id := range []int64{101, 102} {
				score, err := client.ZScore(ctx, spec.indexKey, strconv.FormatInt(id, 10)).Result()
				require.NoError(t, err)
				require.Greater(t, int64(score), now, "remaining requests or waiters keep their active index")
			}
			_, err = client.ZScore(ctx, spec.indexKey, "103").Result()
			require.ErrorIs(t, err, redis.Nil, "an empty expired index member must be removed")
		})
	}
}
