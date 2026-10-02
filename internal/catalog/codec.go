package catalog

import (
	"bytes"
	"encoding/hex"

	"google.golang.org/protobuf/proto"

	"github.com/inode64/fsledger/internal/catalog/recordpb"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
)

func packRecord(record integrity.Record) ([]byte, error) {
	digest, err := hex.DecodeString(record.Hash)
	if err != nil {
		return nil, fault.Wrap("encode record", err)
	}

	message := &recordpb.Record{
		Type:             record.Type,
		AttributesStatus: record.AttributesStatus,
		Algorithm:        record.Algorithm,
		Hash:             digest,
		Target:           record.Target,
		Acl:              packAttrs(record.ACL),
		Xattrs:           packAttrs(record.Xattrs),
		Size:             record.Size,
		Inode:            record.Inode,
		Mtime:            record.Mtime,
		Ctime:            record.Ctime,
		Btime:            record.Btime,
		HashedAt:         record.HashedAt,
		Observed:         record.Observed,
		Nlink:            record.Nlink,
		Atime:            record.Atime,
		Device:           record.Device,
		Gid:              record.GID,
		Uid:              record.UID,
		Mode:             record.Mode,
		NoAtime:          record.NoAtime,
		HasBtime:         record.HasBtime,
		TimesInSeconds:   record.TimesInSeconds,
	}
	data, err := proto.Marshal(message)

	return data, fault.Wrap("encode record", err)
}

// Observation time alone does not change evidence. Hash freshness and every filesystem
// field still participate, including fields not selected by the current comparison policy.
func packObservation(record, previous integrity.Record, oldData []byte) ([]byte, error) {
	if oldData != nil && sameObservation(record, previous) {
		return oldData, nil
	}

	return packRecord(record)
}

func sameObservation(first, second integrity.Record) bool {
	return sameIdentity(first, second) && sameTimes(first, second) &&
		bytes.Equal(first.Target, second.Target) &&
		integrity.AttributesEqual(first.ACL, second.ACL) &&
		integrity.AttributesEqual(first.Xattrs, second.Xattrs)
}

func sameIdentity(first, second integrity.Record) bool {
	return first.Type == second.Type && first.AttributesStatus == second.AttributesStatus &&
		first.Algorithm == second.Algorithm && first.Hash == second.Hash &&
		first.Size == second.Size && first.Inode == second.Inode && first.Device == second.Device &&
		first.Nlink == second.Nlink && first.UID == second.UID && first.GID == second.GID && first.Mode == second.Mode
}

func sameTimes(first, second integrity.Record) bool {
	return first.Mtime == second.Mtime && first.Ctime == second.Ctime && first.Btime == second.Btime &&
		first.Atime == second.Atime && first.HashedAt == second.HashedAt && first.NoAtime == second.NoAtime &&
		first.HasBtime == second.HasBtime && first.TimesInSeconds == second.TimesInSeconds
}

func packAttrs(attributes []integrity.Attribute) []*recordpb.Attribute {
	out := make([]*recordpb.Attribute, len(attributes))
	for index, attribute := range attributes {
		out[index] = &recordpb.Attribute{Name: attribute.Name, Value: attribute.Value}
	}

	return out
}

func unpack(data, path []byte) (integrity.Record, error) {
	message := &recordpb.Record{}

	err := proto.Unmarshal(data, message)
	if err != nil {
		return integrity.Record{}, fault.Wrap("decode record", err)
	}

	record := integrity.Record{
		Path:             append([]byte(nil), path...),
		Type:             message.GetType(),
		AttributesStatus: message.GetAttributesStatus(),
		Algorithm:        message.GetAlgorithm(),
		Hash:             hex.EncodeToString(message.GetHash()),
		Target:           message.GetTarget(),
		ACL:              unpackAttrs(message.GetAcl()),
		Xattrs:           unpackAttrs(message.GetXattrs()),
		Size:             message.GetSize(),
		Inode:            message.GetInode(),
		Mtime:            message.GetMtime(),
		Ctime:            message.GetCtime(),
		Btime:            message.GetBtime(),
		HashedAt:         message.GetHashedAt(),
		Observed:         message.GetObserved(),
		Nlink:            message.GetNlink(),
		Atime:            message.GetAtime(),
		Device:           message.GetDevice(),
		GID:              message.GetGid(),
		UID:              message.GetUid(),
		Mode:             message.GetMode(),
		NoAtime:          message.GetNoAtime(),
		HasBtime:         message.GetHasBtime(),
		TimesInSeconds:   message.GetTimesInSeconds(),
	}

	return record, nil
}

func unpackAttrs(attributes []*recordpb.Attribute) []integrity.Attribute {
	if len(attributes) == 0 {
		return nil
	}

	out := make([]integrity.Attribute, len(attributes))
	for index, attribute := range attributes {
		out[index] = integrity.Attribute{Name: attribute.GetName(), Value: attribute.GetValue()}
	}

	return out
}
