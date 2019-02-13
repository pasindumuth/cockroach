// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package kv

import (
	"context"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	opentracing "github.com/opentracing/opentracing-go"
)

// txnHeartbeater is a txnInterceptor in charge of a transaction's heartbeat
// loop. Transaction coordinators heartbeat their transaction record
// periodically to indicate the liveness of their transaction. Other actors like
// concurrent transactions and GC processes observe a transaction record's last
// heartbeat time to learn about its disposition and to determine whether it
// should be considered abandoned. When a transaction is considered abandoned,
// other actors are free to abort it at will. As such, it is important for a
// transaction coordinator to heartbeat its transaction record with a
// periodicity well below the abandonment threshold.
//
// Transaction coordinators only need to perform heartbeats for transactions
// that risk running for longer than the abandonment duration. For transactions
// that finish well beneath this time, a heartbeat will never be sent and the
// EndTransaction request will create and immediately finalize the transaction.
// However, for transactions that live long enough that they risk running into
// issues with other's perceiving them as abandoned, the first HeartbeatTxn
// request they send will create the transaction record in the PENDING state.
// Future heartbeats will update the transaction record to indicate
// progressively larger heartbeat timestamps.
//
// NOTE: there are other mechanisms by which concurrent actors could determine
// the liveness of transactions. One proposal is to have concurrent actors
// communicate directly with transaction coordinators themselves. This would
// avoid the need for transaction heartbeats and the PENDING transaction state
// entirely. Another proposal is to detect abandoned transactions and failed
// coordinators at an entirely different level - by maintaining a node health
// plane. This would function under the idea that if the node a transaction's
// coordinator is running on is alive then that transaction is still in-progress
// unless it specifies otherwise. These are both approaches we could consider in
// the future.
//
// TODO(nvanbenschoten): Unit test this file.
type txnHeartbeater struct {
	log.AmbientContext

	// wrapped is the next sender in the stack
	wrapped lockedSender
	// gatekeeper is the sender to which heartbeat requests need to be sent. It is
	// set to the gatekeeper interceptor, so sending directly to it will bypass
	// all the other interceptors; heartbeats don't need them and they can only
	// heart - we don't want heartbeats to get sequence numbers or to check any
	// intents. Note that the async rollbacks that this interceptor sometimes
	// sends got through `wrapped`, not directly through `gatekeeper`.
	gatekeeper lockedSender

	st                *cluster.Settings
	clock             *hlc.Clock
	heartbeatInterval time.Duration
	metrics           *TxnMetrics

	// stopper is the TxnCoordSender's stopper. Used to stop the heartbeat loop
	// when quiescing.
	stopper *stop.Stopper

	// asyncAbortCallbackLocked is called when the heartbeat loop shuts itself
	// down because it has detected the transaction to be aborted. The intention
	// is to notify the TxnCoordSender to shut itself down.
	asyncAbortCallbackLocked func(context.Context)

	// When set to true, the transaction will always send a BeginTxn request to
	// lay down a transaction record as early as possible.
	eagerRecord bool

	// mu contains state protected by the TxnCoordSender's mutex.
	mu struct {
		sync.Locker

		// txnEnd is closed when the transaction is aborted or committed, terminating
		// the heartbeat loop. Nil if the heartbeat loop is not running.
		txnEnd chan struct{}

		// txn is a reference to the TxnCoordSender's proto.
		txn *roachpb.Transaction

		// finalErr, if set, will be returned by all subsequent SendLocked() calls,
		// except rollbacks.
		finalErr *roachpb.Error

		// needBeginTxn dictates whether a BeginTxn request is to be prepended to a
		// write batch. It starts as set and then gets unset when the BeginTxn is
		// sent. It gets reset on epoch increment, as it's possible that the
		// retriable error was generated by the BeginTxn batch and the transaction
		// record has not been written.
		// We could be smarter about not resetting this if there's ever been a
		// successful BeginTxn (in which case we know that there is a txn record)
		// but as of May 2018 we don't do that. Note that the server accepts a
		// BeginTxn with a higher epoch if a transaction record already exists.
		// TODO(nvanbenschoten): Once we stop sending BeginTxn entirely (v2.3)
		// we can get rid of this. For now, we keep it to ensure compatibility.
		needBeginTxn bool
	}
}

// init initializes the txnHeartbeater. This method exists instead of a
// constructor because txnHeartbeaters live in a pool in the TxnCoordSender.
func (h *txnHeartbeater) init(
	mu sync.Locker,
	txn *roachpb.Transaction,
	st *cluster.Settings,
	clock *hlc.Clock,
	heartbeatInterval time.Duration,
	gatekeeper lockedSender,
	metrics *TxnMetrics,
	stopper *stop.Stopper,
	asyncAbortCallbackLocked func(context.Context),
) {
	h.stopper = stopper
	h.st = st
	h.clock = clock
	h.heartbeatInterval = heartbeatInterval
	h.metrics = metrics
	h.mu.Locker = mu
	h.mu.txn = txn
	h.mu.needBeginTxn = true
	h.gatekeeper = gatekeeper
	h.asyncAbortCallbackLocked = asyncAbortCallbackLocked
}

// SendLocked is part of the txnInteceptor interface.
func (h *txnHeartbeater) SendLocked(
	ctx context.Context, ba roachpb.BatchRequest,
) (*roachpb.BatchResponse, *roachpb.Error) {
	// If finalErr is set, we reject everything but rollbacks.
	if h.mu.finalErr != nil {
		singleRollback := ba.IsSingleEndTransactionRequest() &&
			!ba.Requests[0].GetInner().(*roachpb.EndTransactionRequest).Commit
		if !singleRollback {
			return nil, h.mu.finalErr
		}
	}

	firstWriteIdx, pErr := firstWriteIndex(&ba)
	if pErr != nil {
		return nil, pErr
	}
	haveTxnWrite := firstWriteIdx != -1
	_, haveEndTxn := ba.GetArg(roachpb.EndTransaction)

	addedBeginTxn := false
	needBeginTxn := haveTxnWrite && h.mu.needBeginTxn
	if needBeginTxn {
		h.mu.needBeginTxn = false
		// From now on, all requests need to be checked against the AbortCache on
		// the server side. We also conservatively update the current request,
		// although I'm not sure if that's necessary.
		h.mu.txn.Writing = true
		ba.Txn.Writing = true

		// Set txn key based on the key of the first transactional write if not
		// already set. If we're in a restart, make sure we keep the anchor key the
		// same.
		if len(h.mu.txn.Key) == 0 {
			anchor := ba.Requests[firstWriteIdx].GetInner().Header().Key
			h.mu.txn.Key = anchor
			// Put the anchor also in the ba's copy of the txn, since this batch was
			// prepared before we had an anchor.
			ba.Txn.Key = anchor
		}

		if h.eagerRecord || !h.st.Version.IsActive(cluster.VersionLazyTxnRecord) {
			addedBeginTxn = true

			// Set the key in the begin transaction request to the txn's anchor key.
			bt := &roachpb.BeginTransactionRequest{
				RequestHeader: roachpb.RequestHeader{
					Key: h.mu.txn.Key,
				},
			}

			// Inject the new request before the first write position, taking care to
			// avoid unnecessary allocations.
			oldRequests := ba.Requests
			ba.Requests = make([]roachpb.RequestUnion, len(ba.Requests)+1)
			copy(ba.Requests, oldRequests[:firstWriteIdx])
			ba.Requests[firstWriteIdx].MustSetInner(bt)
			copy(ba.Requests[firstWriteIdx+1:], oldRequests[firstWriteIdx:])
		}

		// Start the heartbeat loop.
		// Note that we don't do it for 1PC txns: they only leave intents around on
		// retriable errors if the batch has been split between ranges. We consider
		// that unlikely enough so we prefer to not pay for a goroutine.
		//
		// Note that we don't start the heartbeat loop if the loop is already
		// running. That can happen because we send BeginTransaction again after
		// retriable errors.
		if h.mu.txnEnd == nil && !haveEndTxn {
			if err := h.startHeartbeatLoopLocked(ctx); err != nil {
				h.mu.finalErr = roachpb.NewError(err)
				return nil, h.mu.finalErr
			}
		}
	}

	// Forward the batch through the wrapped lockedSender.
	br, pErr := h.wrapped.SendLocked(ctx, ba)

	// If we inserted a begin transaction request, remove it here.
	if addedBeginTxn {
		if br != nil && br.Responses != nil {
			br.Responses = append(br.Responses[:firstWriteIdx], br.Responses[firstWriteIdx+1:]...)
		}
		// Handle case where inserted begin txn confused an indexed error.
		if pErr != nil && pErr.Index != nil {
			idx := pErr.Index.Index
			if idx == int32(firstWriteIdx) {
				// An error was encountered on begin txn; disallow the indexing.
				pErr.Index = nil
			} else if idx > int32(firstWriteIdx) {
				// An error was encountered after begin txn; decrement index.
				pErr.SetErrorIndex(idx - 1)
			}
		}
	}

	return br, pErr
}

// setWrapped is part of the txnInteceptor interface.
func (h *txnHeartbeater) setWrapped(wrapped lockedSender) {
	h.wrapped = wrapped
}

// populateMetaLocked is part of the txnInteceptor interface.
func (h *txnHeartbeater) populateMetaLocked(*roachpb.TxnCoordMeta) {}

// augmentMetaLocked is part of the txnInteceptor interface.
func (h *txnHeartbeater) augmentMetaLocked(roachpb.TxnCoordMeta) {}

// epochBumpedLocked is part of the txnInteceptor interface.
func (h *txnHeartbeater) epochBumpedLocked() {
	h.mu.needBeginTxn = true
}

// closeLocked is part of the txnInteceptor interface.
func (h *txnHeartbeater) closeLocked() {
	// If the heartbeat loop has already finished, there's nothing more to do.
	if h.mu.txnEnd == nil {
		return
	}
	close(h.mu.txnEnd)
	h.mu.txnEnd = nil
}

// startHeartbeatLoopLocked starts a heartbeat loop in a different goroutine.
func (h *txnHeartbeater) startHeartbeatLoopLocked(ctx context.Context) error {
	if h.mu.txnEnd != nil {
		log.Fatal(ctx, "attempting to start a second heartbeat loop ")
	}

	log.VEventf(ctx, 2, "coordinator spawns heartbeat loop")
	h.mu.txnEnd = make(chan struct{})

	// Create a new context so that the heartbeat loop doesn't inherit the
	// caller's cancelation.
	// We want the loop to run in a span linked to the current one, though, so we
	// put our span in the new context and expect RunAsyncTask to fork it
	// immediately.
	hbCtx := h.AnnotateCtx(context.Background())
	hbCtx = opentracing.ContextWithSpan(hbCtx, opentracing.SpanFromContext(ctx))

	return h.stopper.RunAsyncTask(
		hbCtx, "kv.TxnCoordSender: heartbeat loop", func(ctx context.Context) {
			h.heartbeatLoop(ctx)
		})
}

// heartbeatLoop periodically sends a HeartbeatTxn request to the transaction
// record, stopping in the event the transaction is aborted or committed after
// attempting to resolve the intents.
func (h *txnHeartbeater) heartbeatLoop(ctx context.Context) {
	var tickChan <-chan time.Time
	{
		ticker := time.NewTicker(h.heartbeatInterval)
		tickChan = ticker.C
		defer ticker.Stop()
	}

	var finalErr *roachpb.Error
	defer func() {
		h.mu.Lock()
		// Prevent future SendLocked() calls.
		if finalErr != nil {
			h.mu.finalErr = finalErr
		}
		if h.mu.txnEnd != nil {
			h.mu.txnEnd = nil
		}
		h.mu.Unlock()
	}()

	var closer <-chan struct{}
	{
		h.mu.Lock()
		closer = h.mu.txnEnd
		h.mu.Unlock()
		if closer == nil {
			return
		}
	}
	// Loop with ticker for periodic heartbeats.
	for {
		select {
		case <-tickChan:
			if !h.heartbeat(ctx) {
				// This error we're generating here should not be seen by clients. Since
				// the transaction is aborted, they should be rejected before they reach
				// this interceptor.
				finalErr = roachpb.NewErrorf("heartbeat failed fatally")
				return
			}
		case <-closer:
			// Transaction finished normally.
			finalErr = roachpb.NewErrorf("txnHeartbeater already closed")
			return
		case <-h.stopper.ShouldQuiesce():
			finalErr = roachpb.NewErrorf("node already quiescing")
			return
		}
	}
}

// heartbeat sends a HeartbeatTxnRequest to the txn record.
// Errors that carry update txn information (e.g. TransactionAbortedError) will
// update the txn. Other errors are swallowed.
// Returns true if heartbeating should continue, false if the transaction is no
// longer Pending and so there's no point in heartbeating further.
func (h *txnHeartbeater) heartbeat(ctx context.Context) bool {
	// Like with the TxnCoordSender, the locking here is peculiar. The lock is not
	// held continuously throughout this method: we acquire the lock here and
	// then, inside the wrapped.Send() call, the interceptor at the bottom of the
	// stack will unlock until it receives a response.
	h.mu.Lock()
	defer h.mu.Unlock()

	// If the txn is no longer pending, there's nothing for us to heartbeat.
	// This h.heartbeat() call could have raced with a response that updated the
	// status. That response is supposed to have closed the txnHeartbeater.
	if h.mu.txn.Status != roachpb.PENDING {
		if h.mu.txnEnd != nil {
			log.Fatalf(ctx,
				"txn committed or aborted but heartbeat loop hasn't been signaled to stop. txn: %s",
				h.mu.txn)
		}
		return false
	}

	// Clone the txn in order to put it in the heartbeat request.
	txn := h.mu.txn.Clone()

	if txn.Key == nil {
		log.Fatalf(ctx, "attempting to heartbeat txn without anchor key: %v", txn)
	}

	ba := roachpb.BatchRequest{}
	ba.Txn = &txn

	hb := &roachpb.HeartbeatTxnRequest{
		RequestHeader: roachpb.RequestHeader{
			Key: txn.Key,
		},
		Now: h.clock.Now(),
	}
	ba.Add(hb)

	log.VEvent(ctx, 2, "heartbeat")
	br, pErr := h.gatekeeper.SendLocked(ctx, ba)

	var respTxn *roachpb.Transaction
	if pErr != nil {
		log.VEventf(ctx, 2, "heartbeat failed: %s", pErr)

		// If the heartbeat request arrived to find a missing transaction record
		// then we ignore the error. This is possible if the heartbeat loop was
		// started before a BeginTxn request succeeds because of ambiguity in the
		// first write request's response.
		//
		// TODO(nvanbenschoten): Remove this in 2.3.
		if tse, ok := pErr.GetDetail().(*roachpb.TransactionStatusError); ok &&
			tse.Reason == roachpb.TransactionStatusError_REASON_TXN_NOT_FOUND {
			return true
		}

		// We need to be prepared here to handle the case of a
		// TransactionAbortedError with no transaction proto in it.
		//
		// TODO(nvanbenschoten): Make this the only case where we get back an
		// Aborted txn.
		if _, ok := pErr.GetDetail().(*roachpb.TransactionAbortedError); ok {
			h.mu.txn.Status = roachpb.ABORTED
			log.VEventf(ctx, 1, "Heartbeat detected aborted txn. Cleaning up.")
			h.abortTxnAsyncLocked(ctx)
			return false
		}

		respTxn = pErr.GetTxn()
	} else {
		respTxn = br.Responses[0].GetInner().(*roachpb.HeartbeatTxnResponse).Txn
	}

	// Update our txn. In particular, we need to make sure that the client will
	// notice when the txn has been aborted (in which case we'll give them an
	// error on their next request).
	//
	// TODO(nvanbenschoten): It's possible for a HeartbeatTxn request to observe
	// the result of an EndTransaction request and beat it back to the client.
	// This is an issue when a COMMITTED txn record is GCed and later re-written
	// as ABORTED. The coordinator's local status could flip from PENDING to
	// ABORTED (after heartbeat response) to COMMITTED (after commit response).
	// This appears to be benign, but it's still somewhat disconcerting. If this
	// ever causes any issues, we'll need to be smarter about detecting this race
	// on the client and conditionally ignoring the result of heartbeat responses.
	h.mu.txn.Update(respTxn)
	if h.mu.txn.Status != roachpb.PENDING {
		if h.mu.txn.Status == roachpb.ABORTED {
			log.VEventf(ctx, 1, "Heartbeat detected aborted txn. Cleaning up.")
			h.abortTxnAsyncLocked(ctx)
		}
		return false
	}
	return true
}

// abortTxnAsyncLocked send an EndTransaction(commmit=false) asynchronously.
// The asyncAbortCallbackLocked callback is also called.
func (h *txnHeartbeater) abortTxnAsyncLocked(ctx context.Context) {
	if h.mu.txn.Status != roachpb.ABORTED {
		log.Fatalf(ctx, "abortTxnAsyncLocked called for non-aborted txn: %s", h.mu.txn)
	}
	h.asyncAbortCallbackLocked(ctx)
	txn := h.mu.txn.Clone()

	// NB: We use context.Background() here because we don't want a canceled
	// context to interrupt the aborting.
	ctx = h.AnnotateCtx(context.Background())

	// Construct a batch with an EndTransaction request.
	ba := roachpb.BatchRequest{}
	ba.Header = roachpb.Header{Txn: &txn}
	ba.Add(&roachpb.EndTransactionRequest{
		Commit: false,
		// Resolved intents should maintain an abort span entry to prevent
		// concurrent requests from failing to notice the transaction was aborted.
		Poison: true,
	})

	log.VEventf(ctx, 2, "async abort for txn: %s", txn)
	if err := h.stopper.RunAsyncTask(
		ctx, "txnHeartbeater: aborting txn", func(ctx context.Context) {
			// Send the abort request through the interceptor stack. This is important
			// because we need the txnIntentCollector to append intents to the
			// EndTransaction request.
			h.mu.Lock()
			defer h.mu.Unlock()
			_, pErr := h.wrapped.SendLocked(ctx, ba)
			if pErr != nil {
				log.VErrEventf(ctx, 1, "async abort failed for %s: %s ", txn, pErr)
			}
		},
	); err != nil {
		log.Warning(ctx, err)
	}
}

// firstWriteIndex returns the index of the first transactional write in the
// BatchRequest. Returns -1 if the batch has not intention to write. It also
// verifies that if an EndTransactionRequest is included, then it is the last
// request in the batch.
func firstWriteIndex(ba *roachpb.BatchRequest) (int, *roachpb.Error) {
	for i, ru := range ba.Requests {
		args := ru.GetInner()
		if i < len(ba.Requests)-1 /* if not last*/ {
			if _, ok := args.(*roachpb.EndTransactionRequest); ok {
				return -1, roachpb.NewErrorf("%s sent as non-terminal call", args.Method())
			}
		}
		if roachpb.IsTransactionWrite(args) {
			return i, nil
		}
	}
	return -1, nil
}
