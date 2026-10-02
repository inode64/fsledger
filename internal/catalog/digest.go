package catalog

import "github.com/inode64/fsledger/internal/config"

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

		key := digestKey{
			destination: delivery.Destination,
			version:     delivery.Version,
			repository:  delivery.Message.Repository,
			host:        delivery.Message.Host,
			hostname:    delivery.Message.Hostname,
			event:       delivery.Message.Event,
		}

		index, exists := grouped[key]
		if !exists {
			grouped[key] = len(digests)
			digests = append(digests, newDigest(delivery))

			continue
		}

		mergeDelivery(&digests[index], delivery)
	}

	return digests
}

func digestible(event string) bool {
	return event == config.NotificationChange || event == config.NotificationIntegrityViolation
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
