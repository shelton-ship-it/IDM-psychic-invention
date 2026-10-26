package app

import (
	"strconv"
	"time"

	"streamvault/core/httpengine"
	"streamvault/core/storage"
)

// httpSegmentKey é a chave do único segmento do motor HTTP em
// storage.ResumeState.Offsets (HTTP retoma por um offset contíguo).
const httpSegmentKey = "0"

// Chaves da retoma segmentada: "seg_size" e "s<i>" (bytes feitos do segmento i).
const httpSegSizeKey = "seg_size"

// toStorageResume converte o estado de retoma do motor HTTP para o formato
// persistido. O offset fica em Offsets["0"]; ETag/tamanho/Last-Modified são a
// identidade REMOTA usada para detetar mudança do recurso entre sessões.
func toStorageResume(rs httpengine.ResumeState) storage.ResumeState {
	if rs.SegSize > 0 { // download segmentado: um offset POR SEGMENTO
		off := map[string]int64{httpSegSizeKey: rs.SegSize}
		for i, d := range rs.SegDone {
			off["s"+strconv.Itoa(i)] = d
		}
		return storage.ResumeState{TaskID: rs.TaskID, Engine: "http", TempPath: rs.TempPath, Offsets: off,
			RemoteETag: rs.ETag, RemoteSize: rs.Total, RemoteMTime: rs.LastModified, UpdatedAt: time.Now().UTC()}
	}
	return storage.ResumeState{
		TaskID:      rs.TaskID,
		Engine:      "http",
		TempPath:    rs.TempPath,
		Offsets:     map[string]int64{httpSegmentKey: rs.BytesDone},
		RemoteETag:  rs.ETag,
		RemoteSize:  rs.Total,
		RemoteMTime: rs.LastModified,
		UpdatedAt:   time.Now().UTC(),
	}
}

// fromStorageResume faz a conversão inversa. Estados de outro motor ou sem
// offset são recusados (ok=false): o motor recomeça limpo em vez de adivinhar.
func fromStorageResume(rs *storage.ResumeState) (*httpengine.ResumeState, bool) {
	if rs == nil || rs.Engine != "http" {
		return nil, false
	}
	if segSize, seg := rs.Offsets[httpSegSizeKey]; seg {
		if segSize <= 0 || rs.RemoteSize <= 0 || rs.TempPath == "" {
			return nil, false
		}
		n := int((rs.RemoteSize + segSize - 1) / segSize)
		done := make([]int64, n)
		var sum int64
		for i := 0; i < n; i++ {
			d, ok := rs.Offsets["s"+strconv.Itoa(i)]
			if !ok || d < 0 { // estado incompleto: recomeçar limpo em vez de adivinhar
				return nil, false
			}
			done[i], sum = d, sum+d
		}
		if sum <= 0 {
			return nil, false
		}
		return &httpengine.ResumeState{TaskID: rs.TaskID, TempPath: rs.TempPath, BytesDone: sum,
			Total: rs.RemoteSize, ETag: rs.RemoteETag, LastModified: rs.RemoteMTime, CapturedAt: rs.UpdatedAt,
			SegSize: segSize, SegDone: done}, true
	}
	off, ok := rs.Offsets[httpSegmentKey]
	if !ok || off <= 0 || rs.TempPath == "" {
		return nil, false
	}
	return &httpengine.ResumeState{
		TaskID:       rs.TaskID,
		TempPath:     rs.TempPath,
		BytesDone:    off,
		Total:        rs.RemoteSize,
		ETag:         rs.RemoteETag,
		LastModified: rs.RemoteMTime,
		CapturedAt:   rs.UpdatedAt,
	}, true
}
