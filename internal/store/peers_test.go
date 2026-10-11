package store

import (
	"bytes"
	"errors"
	"testing"

	"filesync/internal/chunk"
)

func TestStore_PeerRoundTripAndRevoke(t *testing.T) {
	s := openTestStore(t)
	p := Peer{DeviceID: "DEVA", Name: "laptop", CertFingerprint: "AAAA BBBB",
		Addresses: []string{"192.168.1.5:22000"}, PairedAt: 777}
	if err := s.PutPeer(p); err != nil {
		t.Fatalf("PutPeer: %v", err)
	}
	got, ok, err := s.GetPeer("DEVA")
	if err != nil || !ok {
		t.Fatalf("GetPeer: ok=%v err=%v", ok, err)
	}
	if got.Name != "laptop" || len(got.Addresses) != 1 || got.Addresses[0] != p.Addresses[0] || got.Revoked || got.PairedAt != 777 {
		t.Fatalf("peer mismatch: %+v", got)
	}
	// Nil addresses store as an empty (non-nil after read) list.
	if err := s.PutPeer(Peer{DeviceID: "DEVB"}); err != nil {
		t.Fatal(err)
	}
	b, _, _ := s.GetPeer("DEVB")
	if b.Addresses == nil || len(b.Addresses) != 0 {
		t.Fatalf("addresses not normalized: %#v", b.Addresses)
	}
	peers, err := s.ListPeers()
	if err != nil || len(peers) != 2 || peers[0].DeviceID != "DEVA" {
		t.Fatalf("ListPeers: %v %+v", err, peers)
	}
	if err := s.RevokePeer("DEVA"); err != nil {
		t.Fatalf("RevokePeer: %v", err)
	}
	if err := s.RevokePeer("DEVA"); err != nil {
		t.Fatalf("RevokePeer again (idempotent): %v", err)
	}
	got, _, _ = s.GetPeer("DEVA")
	if !got.Revoked {
		t.Fatal("peer not revoked")
	}
	if err := s.RevokePeer("GHOST"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RevokePeer unknown: %v, want ErrNotFound", err)
	}
	if _, ok, _ := s.GetPeer("GHOST"); ok {
		t.Fatal("unknown peer reported present")
	}
}

func TestStore_JournalAndTransferResume(t *testing.T) {
	s := openTestStore(t)
	if err := s.JournalSet("k", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.JournalGet("k")
	if err != nil || !ok || string(v) != "v1" {
		t.Fatalf("JournalGet: %q ok=%v err=%v", v, ok, err)
	}
	v[0] = 'X' // mutating the returned copy must not corrupt the store
	v2, _, _ := s.JournalGet("k")
	if string(v2) != "v1" {
		t.Fatal("journal value is not a copy")
	}
	if _, ok, _ := s.JournalGet("missing"); ok {
		t.Fatal("missing journal key reported present")
	}

	// Transfer journal with a real resume bitmap: 5 chunks, 1 and 3 done.
	bm := chunk.NewBitmap(5)
	bm.Set(1)
	bm.Set(3)
	raw, err := bm.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	j := TransferJournal{Path: "big.bin", Size: 5 << 20, TotalChunks: 5, Bitmap: raw, UpdatedBy: "DEVA"}
	if err := s.PutTransfer(j); err != nil {
		t.Fatalf("PutTransfer: %v", err)
	}
	got, ok, err := s.GetTransfer("big.bin")
	if err != nil || !ok {
		t.Fatalf("GetTransfer: ok=%v err=%v", ok, err)
	}
	if got.Size != j.Size || got.TotalChunks != 5 || !bytes.Equal(got.Bitmap, raw) {
		t.Fatalf("transfer mismatch: %+v", got)
	}
	var back chunk.Bitmap
	if err := back.UnmarshalBinary(got.Bitmap); err != nil {
		t.Fatal(err)
	}
	if missing := back.Missing(); len(missing) != 3 || missing[0] != 0 || missing[1] != 2 || missing[2] != 4 {
		t.Fatalf("resume missing = %v, want [0 2 4]", missing)
	}
	list, err := s.ListTransfers()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListTransfers: %v %+v", err, list)
	}
	if err := s.ClearTransfer("big.bin"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetTransfer("big.bin"); ok {
		t.Fatal("transfer survived ClearTransfer")
	}
	list, _ = s.ListTransfers()
	if len(list) != 0 {
		t.Fatalf("transfers after clear: %+v", list)
	}
}
