package knowledge

import (
	"errors"
	"io"
	"os"
)

// The descriptor metadata belongs to the bytes read, even if the path is
// replaced concurrently. Both the initial size and a growing file are bounded.
func readKnowledgeFile(path string, maximum int) ([]byte, os.FileInfo, error) {
	file, info, err := openKnowledgeFile(path, maximum)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > maximum || !sameKnowledgeFile(info, after) {
		return nil, nil, errors.New("knowledge file changed or exceeded its byte budget while reading")
	}
	return raw, info, nil
}

func sameKnowledgeFile(first, second os.FileInfo) bool {
	return first != nil && second != nil && first.Mode().IsRegular() && second.Mode().IsRegular() && os.SameFile(first, second) && first.Size() == second.Size() && first.ModTime().Equal(second.ModTime())
}

func openKnowledgeFile(path string, maximum int) (*os.File, os.FileInfo, error) {
	prior, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !prior.Mode().IsRegular() || prior.Size() > int64(maximum) {
		return nil, nil, errors.New("knowledge file exceeds its regular-file byte budget")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if !sameKnowledgeFile(prior, info) {
		file.Close()
		return nil, nil, errors.New("knowledge file changed while opening")
	}

	return file, info, nil
}
