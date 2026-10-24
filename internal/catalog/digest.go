package catalog

import (
	"cmp"
	"context"
	"slices"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/resource"
)

// Digest contains the complete deliveries represented by one transport message.
type Digest struct {
	Destination string
	Version     string
	Deliveries  []Delivery
	Message     Message
}

type digestKey struct {
	destination string
	version     string
	repository  string
	host        string
	hostname    string
	event       string
	progress    Progress
}

// MergeMessages groups digestible events by routing identity while preserving evidence.
// Operational errors and recoveries remain independent messages.
func MergeMessages(deliveries []Delivery) []Digest {
	digests := make([]Digest, 0, len(deliveries))
	grouped := make(map[digestKey]int)

	for _, delivery := range deliveries {
		if !digestible(delivery.Message.Event) {
			digests = append(digests, newDigest(delivery))

			continue
		}

		key := deliveryDigestKey(delivery)

		index, exists := grouped[key]
		if !exists {
			grouped[key] = len(digests)
			digests = append(digests, newDigest(delivery))

			continue
		}

		digests[index].Deliveries = append(digests[index].Deliveries, delivery)
	}

	for index, digest := range digests {
		if len(digest.Deliveries) == 1 {
			continue
		}

		// RetryMany gives the deliveries one due time, which can change their order in Due.
		// Canonicalize the first rendering too, while retaining the order between digests.
		slices.SortFunc(digest.Deliveries, func(first, second Delivery) int { return cmp.Compare(first.ID, second.ID) })

		digests[index] = newDigest(digest.Deliveries[0])
		for _, delivery := range digest.Deliveries[1:] {
			mergeDelivery(&digests[index], delivery)
		}
	}

	return digests
}

func deliveryDigestKey(delivery Delivery) digestKey {
	return digestKey{
		destination: delivery.Destination,
		version:     delivery.Version,
		repository:  delivery.Message.Repository,
		host:        delivery.Message.Host,
		hostname:    delivery.Message.Hostname,
		event:       delivery.Message.Event,
		// A partly delivered rendering must resume with its own deliveries: adding new ones
		// changes its fingerprint and would resend the parts already accepted.
		progress: delivery.Progress,
	}
}

// resumeDigests completes partly delivered groups selected by Due, even across its page limit.
// Only this recovery path reads the outbox: its persisted fingerprints identify the original members,
// including exhausted ones that no longer have a due key. The caller holds store.mutex.
//
//nolint:gocognit // Selection, reconstruction and exhaustion share one locked recovery operation.
func (store *Store) resumeDigests(ctx context.Context, due []Delivery) ([]Delivery, error) {
	groups := make(map[digestKey][]Delivery)

	for _, delivery := range due {
		if digestible(delivery.Message.Event) && delivery.Progress.Fingerprint != "" {
			groups[deliveryDigestKey(delivery)] = nil
		}
	}

	if len(groups) == 0 {
		return due, nil
	}

	err := store.iterate(ctx, outboxPrefix, func(_, data []byte) error {
		delivery, err := decodeDelivery(data)
		if err != nil {
			return err
		}

		key := deliveryDigestKey(delivery)
		if _, selected := groups[key]; selected {
			groups[key] = append(groups[key], delivery)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	result := make([]Delivery, 0, len(due))

	for _, delivery := range due {
		key := deliveryDigestKey(delivery)

		group, selected := groups[key]
		if !selected {
			result = append(result, delivery)

			continue
		}

		if group == nil {
			continue
		}

		groups[key] = nil

		if slices.ContainsFunc(group, func(member Delivery) bool { return member.Exhausted }) {
			// Sending only the remaining members changes the rendering. Keep the whole group
			// queued until a restart retries it, just as for an exhausted individual delivery.
			err = store.exhaustDigest(group)
			if err != nil {
				return nil, err
			}

			continue
		}

		result = append(result, group...)
	}

	return result, nil
}

func (store *Store) exhaustDigest(deliveries []Delivery) error {
	transaction := store.begin()
	defer resource.Close(transaction.batch)

	for _, delivery := range deliveries {
		if delivery.Exhausted {
			continue
		}

		transaction.remove(deliveryDue(delivery))
		delivery.Exhausted = true
		transaction.put(numberKey(outboxPrefix, delivery.ID), delivery)
		transaction.state.Stats.ExhaustedNotifications++
	}

	return transaction.commit()
}

func digestible(event string) bool {
	return event == config.NotificationChange
}

func newDigest(delivery Delivery) Digest {
	message := delivery.Message
	message.Paths = append([]string(nil), delivery.Message.Paths...)
	message.Details = appendDetails(nil, delivery.Message.Details, delivery.Message.ChangeID)

	digest := Digest{
		Destination: delivery.Destination,
		Version:     delivery.Version,
		Deliveries:  []Delivery{delivery},
		Message:     message,
	}

	return digest
}

func mergeDelivery(digest *Digest, delivery Delivery) {
	digest.Deliveries = append(digest.Deliveries, delivery)
	mergeMessage(&digest.Message, delivery.Message)
}

func mergeMessage(target *Message, source Message) {
	if target.ChangeID != source.ChangeID {
		target.ChangeID = ""
	}

	target.Paths = append(target.Paths, source.Paths...)
	target.Details = appendDetails(target.Details, source.Details, source.ChangeID)
	target.Count += source.Count
}

func appendDetails(target, details []Detail, identifier string) []Detail {
	for _, detail := range details {
		if detail.ChangeID == "" {
			detail.ChangeID = identifier
		}

		target = append(target, detail)
	}

	return target
}
