package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// ---- shared fakes ----

// fakeResolver resolves every chat id to a channel peer with freshHash.
type fakeResolver struct {
	freshHash int64
	byName    map[int64]tg.InputPeerClass
}

func (f *fakeResolver) ResolveFromChatID(ctx context.Context, chatID int64) (tg.InputPeerClass, error) {
	if p, ok := f.byName[chatID]; ok {
		return p, nil
	}
	return &tg.InputPeerChannel{ChannelID: chatID + 1000, AccessHash: f.freshHash}, nil
}

func (f *fakeResolver) ResolveUserInChannel(ctx context.Context, c tg.InputChannelClass, userID int64) (tg.InputPeerClass, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeResolver) ResolveUserFromMessage(ctx context.Context, peer tg.InputPeerClass, msgID int, userID int64) (tg.InputPeerClass, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeResolver) ResolveUsername(ctx context.Context, username string) (tg.InputPeerClass, error) {
	return nil, errors.New("no such username")
}

// fakeHost carries just what runTask/afterRun touch.
type fakeHost struct {
	plugin.Host
	api *tg.Client
	res *fakeResolver
}

func (f *fakeHost) API() *tg.Client                   { return f.api }
func (f *fakeHost) PeerResolver() plugin.PeerResolver { return f.res }
func (f *fakeHost) SelfID() int64                     { return 1 }
func (f *fakeHost) Lang(userID int64) string          { return "zh-CN" }

// fakeInvoker answers messages.sendMessage: first call fails with the
// configured error, later calls succeed. It records the peers used.
type fakeInvoker struct {
	failWith error
	peers    []tg.InputPeerClass
}

func (f *fakeInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	req, ok := input.(*tg.MessagesSendMessageRequest)
	if !ok {
		return fmt.Errorf("unexpected request %T", input)
	}
	f.peers = append(f.peers, req.Peer)
	if f.failWith != nil {
		err := f.failWith
		f.failWith = nil
		return err
	}
	if ub, ok := output.(*tg.UpdatesBox); ok {
		ub.Updates = &tg.UpdateShortSentMessage{ID: 500, Date: 1}
	}
	return nil
}

// ---- finding 1: stale peer re-resolve + retry + write-back ----

func TestRunTaskReResolvesStalePeer(t *testing.T) {
	inv := &fakeInvoker{failWith: tgerr.New(400, "PEER_ID_INVALID")}
	host := &fakeHost{api: tg.NewClient(inv), res: &fakeResolver{freshHash: 999}}
	p := New()
	p.host = host
	p.dir = t.TempDir()

	stored := &Task{
		ID: 1, Type: typeSend, Cron: "0 0 2 * * *",
		ChatID: -100123, Chat: "@somechan", Message: "hi",
		Peer: &peerRef{Type: "channel", ID: 123, AccessHash: 1},
	}
	p.tasks = []*Task{stored}
	p.next[1] = time.Now().Add(-time.Second)

	snap := *stored
	res, newPeer, err := p.runTask(context.Background(), &snap)
	if err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if res == "" {
		t.Fatalf("empty result")
	}
	if newPeer == nil || newPeer.AccessHash != 999 {
		t.Fatalf("newPeer=%v, want re-resolved hash 999", newPeer)
	}
	if len(inv.peers) != 2 {
		t.Fatalf("sends=%d, want failed attempt + retry", len(inv.peers))
	}
	// The retry must use the fresh peer, not the stale one.
	if ch, ok := inv.peers[1].(*tg.InputPeerChannel); !ok || ch.AccessHash != 999 {
		t.Fatalf("retry peer=%v, want fresh hash", inv.peers[1])
	}

	// Write-back persists the fresh peer on the stored task.
	p.afterRun(stored.ID, res, newPeer, nil)
	if stored.Peer == nil || stored.Peer.AccessHash != 999 {
		t.Fatalf("stored task peer not updated: %+v", stored.Peer)
	}
	if stored.LastError != "" || stored.LastResult != res {
		t.Fatalf("accounting wrong: %q / %q", stored.LastError, stored.LastResult)
	}
}

func TestRunTaskNoRetryWhenReResolveYieldsSamePeer(t *testing.T) {
	inv := &fakeInvoker{failWith: tgerr.New(400, "PEER_ID_INVALID")}
	host := &fakeHost{api: tg.NewClient(inv), res: &fakeResolver{}}
	p := New()
	p.host = host

	// ChatID resolves to the very peer the task already stores.
	host.res.byName = map[int64]tg.InputPeerClass{-200: &tg.InputPeerChannel{ChannelID: 7, AccessHash: 1}}
	tsk := &Task{ID: 2, Type: typeSend, Cron: "0 0 2 * * *", ChatID: -200, Message: "hi",
		Peer: &peerRef{Type: "channel", ID: 7, AccessHash: 1}}
	_, newPeer, err := p.runTask(context.Background(), tsk)
	if err == nil {
		t.Fatalf("expected the original error to surface")
	}
	if newPeer != nil {
		t.Fatalf("newPeer should be nil when re-resolve changes nothing")
	}
	if len(inv.peers) != 1 {
		t.Fatalf("sends=%d, want no blind retry", len(inv.peers))
	}
}

func TestRunTaskDoesNotRetryFloodWait(t *testing.T) {
	inv := &fakeInvoker{failWith: tgerr.New(420, "FLOOD_WAIT_5")}
	host := &fakeHost{api: tg.NewClient(inv), res: &fakeResolver{freshHash: 999}}
	p := New()
	p.host = host

	tsk := &Task{ID: 3, Type: typeSend, Cron: "0 0 2 * * *", ChatID: -100123, Message: "hi",
		Peer: &peerRef{Type: "channel", ID: 123, AccessHash: 1}}
	_, newPeer, err := p.runTask(context.Background(), tsk)
	if err == nil {
		t.Fatalf("expected flood wait error")
	}
	if newPeer != nil {
		t.Fatalf("flood wait must not trigger re-resolve")
	}
	if len(inv.peers) != 1 {
		t.Fatalf("sends=%d, want exactly one attempt", len(inv.peers))
	}
}

func TestIsStalePeerErr(t *testing.T) {
	for _, tp := range []string{"PEER_ID_INVALID", "CHANNEL_INVALID", "USER_ID_INVALID", "CHANNEL_PRIVATE"} {
		if !isStalePeerErr(tgerr.New(400, tp)) {
			t.Errorf("isStalePeerErr(%s) = false, want true", tp)
		}
	}
	for _, tp := range []string{"FLOOD_WAIT_5", "PEER_FLOOD", "CHAT_WRITE_FORBIDDEN", "SOME_OTHER"} {
		if isStalePeerErr(tgerr.New(400, tp)) {
			t.Errorf("isStalePeerErr(%s) = true, want false", tp)
		}
	}
	if isStalePeerErr(errors.New("plain")) {
		t.Errorf("isStalePeerErr(plain error) = true, want false")
	}
	if !isStalePeerErr(fmt.Errorf("send: %w", tgerr.New(400, "PEER_ID_INVALID"))) {
		t.Errorf("isStalePeerErr(wrapped) = false, want true")
	}
}

func TestSamePeerRef(t *testing.T) {
	a := &peerRef{Type: "channel", ID: 10, AccessHash: 111}
	b := &peerRef{Type: "channel", ID: 10, AccessHash: 111}
	c := &peerRef{Type: "channel", ID: 10, AccessHash: 222} // rotated hash
	if !samePeerRef(a, b) {
		t.Errorf("same refs not equal")
	}
	if samePeerRef(a, c) {
		t.Errorf("different access hash reported same")
	}
	if samePeerRef(a, nil) || samePeerRef(nil, a) {
		t.Errorf("nil ref reported same")
	}
}

// ---- finding 2: del_re pagination must advance past service messages ----

// pageOf builds a history page; negative ids stand for service messages.
func pageOf(ids []int, text string) tg.MessagesMessagesClass {
	msgs := make([]tg.MessageClass, 0, len(ids))
	for _, id := range ids {
		if id < 0 {
			msgs = append(msgs, &tg.MessageService{ID: -id})
			continue
		}
		msgs = append(msgs, &tg.Message{ID: id, Message: text})
	}
	return &tg.MessagesMessagesSlice{Messages: msgs, Count: len(msgs)}
}

func TestScanMatchingAdvancesPastServiceMessages(t *testing.T) {
	// Two full pages of pure service messages followed by a page with one
	// regular message. Before the fix offsetID never moved and the loop
	// hammered the same page until the run timeout.
	re := regexp.MustCompile("spam")
	ids := func(top int) []int { // ids top..top-99, all service (negative)
		out := make([]int, 100)
		for i := range out {
			out[i] = -(top - i)
		}
		return out
	}
	pages := []tg.MessagesMessagesClass{
		pageOf(ids(1200), "spam"),
		pageOf(ids(1100), "spam"),
		pageOf([]int{98}, "spam"),
	}
	calls := 0
	fetch := func(_ context.Context, req *tg.MessagesGetHistoryRequest) (tg.MessagesMessagesClass, error) {
		defer func() { calls++ }()
		if calls >= len(pages) {
			t.Fatalf("fetch called %d times, pagination did not advance", calls+1)
		}
		// Each page after the first must start strictly below the previous
		// page's oldest id, proving offsetID advanced past service entries.
		if calls > 0 {
			prev := pages[calls-1].(*tg.MessagesMessagesSlice).Messages[0].GetID()
			if req.OffsetID >= prev {
				t.Fatalf("call %d: offsetID=%d not below previous page (%d)", calls, req.OffsetID, prev)
			}
		}
		return pages[calls], nil
	}
	got, err := scanMatching(context.Background(), &tg.InputPeerChat{ChatID: 1}, 100, re, fetch)
	if err != nil {
		t.Fatalf("scanMatching: %v", err)
	}
	if len(got) != 1 || got[0] != 98 {
		t.Fatalf("ids=%v, want [98]", got)
	}
	if calls != 3 {
		t.Fatalf("calls=%d, want 3", calls)
	}
}

func TestScanMatchingStopsAtShortPage(t *testing.T) {
	re := regexp.MustCompile("x")
	fetch := func(_ context.Context, _ *tg.MessagesGetHistoryRequest) (tg.MessagesMessagesClass, error) {
		return pageOf([]int{5, 4, 3}, "x"), nil
	}
	ids, err := scanMatching(context.Background(), &tg.InputPeerChat{ChatID: 1}, 100, re, fetch)
	if err != nil {
		t.Fatalf("scanMatching: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("ids=%v, want 3", ids)
	}
}

// ---- finding 3: nextID rollback on failed save ----

func TestAddTaskRollsBackNextIDOnFailedSave(t *testing.T) {
	// dir points at a regular file: MkdirAll fails → saveLocked errors.
	blocked := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New()
	p.dir = blocked
	p.nextID = 5

	p.mu.Lock()
	_, err := p.addTaskLocked(&Task{Type: typeSend, Cron: "0 0 2 * * *", Message: "hi"})
	p.mu.Unlock()
	if err == nil {
		t.Fatalf("save unexpectedly succeeded against blocked dir")
	}
	if len(p.tasks) != 0 {
		t.Fatalf("task not rolled back: %v", p.tasks)
	}
	if p.nextID != 5 {
		t.Fatalf("nextID=%d after failed save, want rollback to 5", p.nextID)
	}
	if _, ok := p.next[5]; ok {
		t.Fatalf("next map entry not rolled back")
	}
}

func TestAddTaskPersistsAndIncrements(t *testing.T) {
	p := New()
	p.dir = t.TempDir()
	p.nextID = 5

	p.mu.Lock()
	_, err := p.addTaskLocked(&Task{Type: typeSend, Cron: "0 0 2 * * *", Message: "hi"})
	p.mu.Unlock()
	if err != nil {
		t.Fatalf("addTaskLocked: %v", err)
	}
	if p.nextID != 6 || len(p.tasks) != 1 || p.tasks[0].ID != 5 {
		t.Fatalf("state=%d/%d/%d, want 6/1/5", p.nextID, len(p.tasks), p.tasks[0].ID)
	}
	var sf storeFile
	if err := readJSON(filepath.Join(p.dir, tasksFile), &sf); err != nil {
		t.Fatalf("readJSON: %v", err)
	}
	if sf.NextID != 6 || len(sf.Tasks) != 1 {
		t.Fatalf("persisted=%+v", sf)
	}
}

// ---- finding 4: debounced accounting writes ----

func TestAccountingDebounce(t *testing.T) {
	dir := t.TempDir()
	p := New()
	p.dir = dir
	p.tasks = []*Task{{ID: 7, Type: typeSend, Cron: "0 0 2 * * *", Message: "hi"}}

	// First accounting change: no disk write yet, just marked dirty.
	p.afterRun(7, "ok", nil, nil)
	p.mu.Lock()
	dirty1 := p.dirty
	p.mu.Unlock()
	if !dirty1 {
		t.Fatalf("expected dirty after first accounting update")
	}
	if _, err := os.Stat(filepath.Join(dir, tasksFile)); !os.IsNotExist(err) {
		t.Fatalf("tasks.json written on first accounting update; debounce broken")
	}

	// Within the window: still no write.
	p.afterRun(7, "ok2", nil, nil)
	if _, err := os.Stat(filepath.Join(dir, tasksFile)); !os.IsNotExist(err) {
		t.Fatalf("tasks.json written inside debounce window")
	}

	// After the window: write happens and dirty clears.
	p.mu.Lock()
	p.lastSave = time.Now().Add(-2 * saveDebounce)
	p.mu.Unlock()
	p.afterRun(7, "ok3", nil, nil)
	p.mu.Lock()
	dirty3 := p.dirty
	p.mu.Unlock()
	if dirty3 {
		t.Fatalf("dirty not cleared after save")
	}
	if _, err := os.Stat(filepath.Join(dir, tasksFile)); err != nil {
		t.Fatalf("tasks.json not written after debounce window: %v", err)
	}

	// Stop flushes remaining accounting changes.
	p.afterRun(7, "ok4", nil, nil)
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	var sf storeFile
	if err := readJSON(filepath.Join(dir, tasksFile), &sf); err != nil {
		t.Fatalf("readJSON: %v", err)
	}
	if len(sf.Tasks) != 1 || sf.Tasks[0].LastResult != "ok4" {
		t.Fatalf("last accounting state not flushed on Stop: %+v", sf.Tasks)
	}
}

func TestFlushStalePersistsAfterWindow(t *testing.T) {
	dir := t.TempDir()
	p := New()
	p.dir = dir
	p.tasks = []*Task{{ID: 9, Type: typeSend, Cron: "0 0 2 * * *", Message: "hi"}}

	p.afterRun(9, "done", nil, nil)
	if _, err := os.Stat(filepath.Join(dir, tasksFile)); !os.IsNotExist(err) {
		t.Fatalf("premature write")
	}
	// Simulate the debounce window elapsing; the scheduler loop calls this.
	p.mu.Lock()
	p.lastSave = time.Now().Add(-saveDebounce - time.Second)
	p.mu.Unlock()
	p.flushStale()
	if _, err := os.Stat(filepath.Join(dir, tasksFile)); err != nil {
		t.Fatalf("flushStale did not persist: %v", err)
	}
	p.mu.Lock()
	still := p.dirty
	p.mu.Unlock()
	if still {
		t.Fatalf("dirty not cleared by flushStale")
	}
}
