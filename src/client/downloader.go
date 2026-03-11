package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	pb "distributed-llama/generated/inference"
)

func downloadModel(ctx context.Context, client pb.CoordinatorServiceClient, storageDir, filename string, totalSize int64) (string, error) {
	destPath := filepath.Join(storageDir, filename)

	if info, err := os.Stat(destPath); err == nil {
		if info.Size() == totalSize {
			log.Printf("[download] %s already cached", filename)
			abs, _ := filepath.Abs(destPath)
			return abs, nil
		}
		log.Printf("[download] size mismatch - re-downloading %s", filename)
	}

	log.Printf("[download] fetching %s (%d bytes)", filename, totalSize)

	stream, err := client.UploadModel(ctx, &pb.UploadModelRequest{Filename: filename})
	if err != nil {
		return "", fmt.Errorf("UploadModel RPC: %w", err)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", destPath, err)
	}
	defer f.Close()

	var received int64
	var lastLogPct int
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = os.Remove(destPath)
			return "", fmt.Errorf("receive chunk: %w", err)
		}
		if _, err := f.WriteAt(chunk.Data, chunk.Offset); err != nil {
			_ = os.Remove(destPath)
			return "", fmt.Errorf("write at %d: %w", chunk.Offset, err)
		}
		received += int64(len(chunk.Data))
		if totalSize > 0 {
			pct := int(float64(received) / float64(totalSize) * 100)
			if pct/10 > lastLogPct/10 {
				log.Printf("[download] %d%% (%d/%d bytes)", pct, received, totalSize)
				lastLogPct = pct
			}
		}
		if chunk.IsEof {
			break
		}
	}

	log.Printf("[download] complete: %s (%d bytes)", filename, received)
	abs, _ := filepath.Abs(destPath)
	return abs, nil
}
