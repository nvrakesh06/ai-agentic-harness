package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLeasePublicationContextUsesCurrentDurableExpiry(t *testing.T) {
	now := time.Now().UTC()
	c := &Controller{now: func() time.Time { return now }}
	ctx, cancel, err := c.leasePublicationContext(context.Background(), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.Equal(now.Add(time.Minute)) {
		t.Fatalf("publication was not fenced at durable lease expiry: %v %t", deadline, ok)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("future durable expiry created an already-canceled publication context: %v", ctx.Err())
	default:
	}
	if _, _, err = c.leasePublicationContext(context.Background(), now); !errors.Is(err, ErrLease) {
		t.Fatalf("expired durable lease authorized publication: %v", err)
	}
}

func TestLeasePublicationContextAlsoFencesProposedAcquisitionExpiry(t *testing.T) {
	now := time.Now().UTC()
	c := &Controller{now: func() time.Time { return now }}
	proposed := now.Add(3 * time.Minute)
	ctx, cancel, err := c.leasePublicationContext(context.Background(), proposed)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.Equal(proposed) {
		t.Fatalf("acquisition publication escaped proposed lease expiry: %v %t", deadline, ok)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("future proposed expiry created an already-canceled publication context: %v", ctx.Err())
	default:
	}
}
