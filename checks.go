package main

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-errors/errors"
	"github.com/itchio/butler/comm"
	"github.com/itchio/wharf/crc32c"
)

type BadSizeErr struct {
	Expected int64
	Actual   int64
}

func (bse *BadSizeErr) Error() string {
	return fmt.Sprintf("size on disk didn't match expected size: wanted %d, got %d", bse.Expected, bse.Actual)
}

type BadHashErr struct {
	Algo     string
	Expected []byte
	Actual   []byte
}

func (bhe *BadHashErr) Error() string {
	return fmt.Sprintf("%s hash mismatch: wanted %x, got %x", bhe.Algo, bhe.Expected, bhe.Actual)
}

func IsIntegrityError(err error) bool {
	if _, ok := err.(*BadSizeErr); ok {
		return true
	}
	if _, ok := err.(*BadHashErr); ok {
		return true
	}

	if original, ok := err.(*errors.Error); ok {
		return IsIntegrityError(original.Err)
	}

	return false
}

func checkIntegrity(header http.Header, contentLength int64, file string) error {
	diskSize := int64(0)
	stats, err := os.Lstat(file)
	if err == nil {
		diskSize = stats.Size()
	}

	// some servers will return a negative content-length, or 0
	// they both mostly mean they didn't know the length of the response
	// at the time the request was made (streaming proxies, for example)
	if contentLength > 0 {
		if diskSize != contentLength {
			return &BadSizeErr{
				Expected: contentLength,
				Actual:   diskSize,
			}
		}
		comm.Debugf("%10s pass (%d bytes)", "size", diskSize)
	}

	return checkHashes(header, file)
}

func checkHashes(header http.Header, file string) error {
	googHashes := header[http.CanonicalHeaderKey("x-goog-hash")]

	for _, googHash := range googHashes {
		tokens := strings.SplitN(googHash, "=", 2)
		hashType := tokens[0]
		hashValue, err := base64.StdEncoding.DecodeString(tokens[1])
		if err != nil {
			comm.Logf("Could not verify %s hash: %s", hashType, err)
			continue
		}

		start := time.Now()
		checked, err := checkHash(hashType, hashValue, file)
		if err != nil {
			return errors.Wrap(err, 1)
		}

		if checked {
			comm.Debugf("%10s pass (took %s)", hashType, time.Since(start))
		} else {
			comm.Debugf("%10s skip (use --thorough to force check)", hashType)
		}
	}

	return nil
}

func checkHash(hashType string, hashValue []byte, file string) (checked bool, err error) {
	checked = true

	switch hashType {
	case "md5":
		if *dlArgs.thorough {
			err = checkHashMD5(hashValue, file)
		} else {
			checked = false
		}
	case "crc32c":
		err = checkHashCRC32C(hashValue, file)
	default:
		checked = false
	}

	if err != nil {
		err = errors.Wrap(err, 1)
	}
	return
}

func checkHashMD5(hashValue []byte, file string) error {
	fr, err := os.Open(file)
	if err != nil {
		return errors.Wrap(err, 1)
	}
	defer fr.Close()

	hasher := md5.New()
	io.Copy(hasher, fr)

	hashComputed := hasher.Sum(nil)
	if !bytes.Equal(hashValue, hashComputed) {
		return &BadHashErr{
			Algo:     "md5",
			Actual:   hashComputed,
			Expected: hashValue,
		}
	}

	return nil
}

func checkHashCRC32C(hashValue []byte, file string) error {
	fr, err := os.Open(file)
	if err != nil {
		return errors.Wrap(err, 1)
	}
	defer fr.Close()

	hasher := crc32.New(crc32c.Table)
	io.Copy(hasher, fr)

	hashComputed := hasher.Sum(nil)
	if !bytes.Equal(hashValue, hashComputed) {
		return &BadHashErr{
			Algo:     "crc32c",
			Actual:   hashComputed,
			Expected: hashValue,
		}
	}

	return nil
}
