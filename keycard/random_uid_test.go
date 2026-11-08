package keycard

import (
	"context"
	"testing"
	"time"
)

func TestIsRandomUID(t *testing.T) {
	for uid, want := range map[string]bool{
		"08A1B2C3": true, "08a1b2c3": true, "08000000": true,
		"AABBCCDD": false, "11223344": false, "": false,
		"08A1B2C3D4E5F6": false, // longer UIDs have no random-ID marker
		"18A1B2C3":       false,
	} {
		if got := isRandomUID(uid); got != want {
			t.Errorf("isRandomUID(%q) = %v, want %v", uid, got, want)
		}
	}
}

func TestLearnDiscardsRandomUIDUntilRepeated(t *testing.T) {
	service, _ := newExclusivityTestService(t)
	service.learnMode = true
	ctx := context.Background()
	listener := service.redis.client.Raw().Subscribe(ctx, "keycard:events")
	defer listener.Close()
	if _, err := listener.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	next := func() string {
		t.Helper()
		waitCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		msg, err := listener.ReceiveMessage(waitCtx)
		if err != nil {
			t.Fatalf("event: %v", err)
		}
		return msg.Payload
	}

	service.learnUID("08A1B2C3")
	if got := next(); got != "card-random-id:08A1B2C3" {
		t.Fatalf("first random tap event = %q", got)
	}
	// A phone presents a new random ID on every tap and is discarded again.
	service.learnUID("08D4E5F6")
	if got := next(); got != "card-random-id:08D4E5F6" {
		t.Fatalf("changed random tap event = %q", got)
	}
	if len(service.newUIDs) != 0 {
		t.Fatalf("changing random IDs were queued: %v", service.newUIDs)
	}
	// A card that repeats its ID is accepted.
	service.learnUID("08D4E5F6")
	if got := next(); got != "card-learned:08D4E5F6" {
		t.Fatalf("repeated random tap event = %q", got)
	}
	if len(service.newUIDs) != 1 || service.newUIDs[0] != "08D4E5F6" {
		t.Fatalf("queued UIDs = %v", service.newUIDs)
	}
	// A normal card is unaffected.
	service.learnUID(user1)
	if got := next(); got != "card-learned:"+user1 {
		t.Fatalf("normal card event = %q", got)
	}
}

func TestRandomUIDWarningDoesNotSurviveModeChange(t *testing.T) {
	service, _ := newExclusivityTestService(t)
	service.learnMode = true
	service.learnUID("08A1B2C3")
	if service.randomUIDPending == "" {
		t.Fatal("random UID warning was not recorded")
	}
	service.masterTeachInMode = false
	service.enterMasterTeachIn()
	if service.randomUIDPending != "" {
		t.Fatal("random UID warning survived entering teach-in")
	}
}

func TestMasterEnrollmentDiscardsRandomUID(t *testing.T) {
	service, _ := newExclusivityTestService(t)
	service.masterBootstrapMode = true
	service.bootstrapMasterUID("08A1B2C3")
	if service.auth.HasMaster() || !service.masterBootstrapMode {
		t.Fatal("bootstrap accepted a random UID on first tap")
	}
	service.bootstrapMasterUID("08A1B2C3")
	if !service.auth.IsMaster("08A1B2C3") || service.masterBootstrapMode {
		t.Fatal("bootstrap rejected a repeated random UID")
	}

	service, _ = newExclusivityTestService(t)
	service.masterTeachInMode = true
	service.teachInMasterUID("08A1B2C3")
	if service.auth.IsMaster("08A1B2C3") {
		t.Fatal("teach-in accepted a random UID on first tap")
	}
	service.teachInMasterUID("08A1B2C3")
	if !service.auth.IsMaster("08A1B2C3") {
		t.Fatal("teach-in rejected a repeated random UID")
	}
}
