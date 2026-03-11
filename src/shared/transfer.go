package shared

import (
	"fmt"
	"io"

	pb "distributed-llama/generated/inference"
)

const ChunkSize = 256 * 1024

func StreamChunks(r io.Reader, totalSize int64, send func(*pb.ModelChunk) error) error {
	buf := make([]byte, ChunkSize)
	var offset int64

	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := &pb.ModelChunk{
				Data:   make([]byte, n),
				Offset: offset,
			}
			copy(chunk.Data, buf[:n])
			offset += int64(n)
			if offset >= totalSize {
				chunk.IsEof = true
			}
			if sendErr := send(chunk); sendErr != nil {
				return fmt.Errorf("send chunk: %w", sendErr)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
	}
	return nil
}
