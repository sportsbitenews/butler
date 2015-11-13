package main

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"golang.org/x/crypto/ssh"
	"gopkg.in/kothar/brotli-go.v0/dec"
	"gopkg.in/kothar/brotli-go.v0/enc"
)

var version = "head"

type butlerError struct {
	Error string
}

type butlerDownloadStatus struct {
	Percent int
}

type butlerMessage struct {
	Message string
}

const bufferSize = 128 * 1024

func main() {
	if len(os.Args) < 2 {
		die("Missing command")
	}
	cmd := os.Args[1]

	switch cmd {
	case "version":
		fmt.Println(fmt.Sprintf("butler version %s", version))
	case "dl":
		dl()
	case "test-ssh":
		testSSH()
	case "test-brotli":
		testBrotli()
	default:
		die("Invalid command")
	}
}

func send(v interface{}) {
	j, _ := json.Marshal(v)
	fmt.Println(string(j))
}

func die(msg string) {
	e := &butlerError{Error: msg}
	send(e)
	os.Exit(1)
}

func msg(msg string) {
	e := &butlerMessage{Message: msg}
	send(e)
}

func dl() {
	if len(os.Args) < 4 {
		die("Missing url or dest for dl command")
	}
	url := os.Args[2]
	dest := os.Args[3]

	tries := 3
	for tries > 0 {
		_, err := tryDl(url, dest)
		if err == nil {
			break
		}

		msg(fmt.Sprintf("While downloading, got error %s", err))
		tries--
		if tries > 0 {
			os.Truncate(dest, 0)
			msg(fmt.Sprintf("Retrying... (%d tries left)", tries))
		}
	}
}

func tryDl(url string, dest string) (int64, error) {
	initialBytes := int64(0)
	stats, err := os.Lstat(dest)
	if err == nil {
		initialBytes = stats.Size()
		msg(fmt.Sprintf("existing file is %d bytes long", initialBytes))
	}

	bytesWritten := initialBytes

	out, _ := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
	defer out.Close()

	client := &http.Client{}
	req, _ := http.NewRequest("HEAD", url, nil)
	resp, err := client.Do(req)
	if initialBytes > 0 && resp.ContentLength == initialBytes {
		msg("all downloaded!")
		googHashes := resp.Header[http.CanonicalHeaderKey("x-goog-hash")]
		if len(googHashes) > 0 {
			msg(fmt.Sprintf("would have %d googHahshes to check!", len(googHashes)))
		}

		return resp.ContentLength, nil
	}

	req, _ = http.NewRequest("GET", url, nil)
	byteRange := fmt.Sprintf("bytes=%d-", bytesWritten)
	msg(fmt.Sprintf("Asking for range %s", byteRange))

	req.Header.Set("Range", byteRange)
	resp, err = client.Do(req)
	if err != nil {
		msg("error on client.Do")
		return 0, err
	}

	if resp.Status[0:1] != "2" {
		return 0, fmt.Errorf("server error: http %s", resp.Status)
	}

	defer resp.Body.Close()
	msg(fmt.Sprintf("Response content length = %d", resp.ContentLength))

	hashes := make(map[string][]byte)

	googHashes := resp.Header[http.CanonicalHeaderKey("x-goog-hash")]
	for i := 0; i < len(googHashes); i++ {
		googHash := googHashes[i]
		tokens := strings.SplitN(googHash, "=", 2)
		hashType := tokens[0]
		hashValue, err := base64.StdEncoding.DecodeString(tokens[1])
		if err != nil {
			msg(fmt.Sprintf("Could not decode base64-encoded %s hash %s because %s, skipping", hashType, tokens[1], err))
			continue
		}
		hashes[hashType] = hashValue
	}

	totalBytes := (initialBytes + resp.ContentLength)
	for {
		n, err := io.CopyN(out, resp.Body, bufferSize)
		bytesWritten += n

		status := &butlerDownloadStatus{
			Percent: int(bytesWritten * 100 / totalBytes)}
		send(status)

		if err != nil {
			if err == io.EOF {
				break
			}
			return 0, err
		}
	}

	out.Close()
	msg(fmt.Sprintf("done downloading"))

	if resp.ContentLength != 0 {
		contentLength := resp.ContentLength + initialBytes
		msg(fmt.Sprintf("checking file size. should be %d, is %d", contentLength, bytesWritten))

		if contentLength != bytesWritten {
			return 0, fmt.Errorf("corrupted downloaded: expected %d bytes, got %d", contentLength, bytesWritten)
		}
	}

	for hashType, hashValue := range hashes {
		if hashType == "md5" {
			msg(fmt.Sprintf("checking %s hash", hashType))
			fr, err := os.Open(dest)
			if err != nil {
				panic(err)
			}
			defer fr.Close()

			hasher := md5.New()
			io.Copy(hasher, fr)

			hashComputed := hasher.Sum(nil)
			if !bytes.Equal(hashValue, hashComputed) {
				msg(fmt.Sprintf("given    = %x", hashValue))
				msg(fmt.Sprintf("computed = %x", hashComputed))
				return 0, fmt.Errorf("corrupted download: %s hash mismatch", hashType)
			}
		} else {
			msg(fmt.Sprintf("skipping %s hash", hashType))
		}
	}

	return bytesWritten, nil
}

func publicKeyFile(file string) ssh.AuthMethod {
	buffer, err := ioutil.ReadFile(file)
	if err != nil {
		return nil
	}

	key, err := ssh.ParsePrivateKey(buffer)
	if err != nil {
		return nil
	}

	log.Println("Our public key is", base64.StdEncoding.EncodeToString(key.PublicKey().Marshal()))
	return ssh.PublicKeys(key)
}

func testSSH() {
	host := "butler.itch.zone"
	port := 2222
	serverString := fmt.Sprintf("%s:%d", host, port)
	fmt.Printf("Trying to connect to %s\n", serverString)

	keyPath := fmt.Sprintf("%s/%s", os.Getenv("HOME"), ".ssh/id_rsa")
	key := publicKeyFile(keyPath)
	auth := []ssh.AuthMethod{key}

	sshConfig := &ssh.ClientConfig{
		User: "butler",
		Auth: auth,
	}

	serverConn, err := ssh.Dial("tcp", serverString, sshConfig)
	if err != nil {
		fmt.Printf("Server dial error: %s\n", err)
		return
	}
	fmt.Printf("Connected!\n")

	ch, _, err := serverConn.OpenChannel("butler", []byte{})
	if err != nil {
		panic(err)
	}

	ch.Write([]byte("Hi"))
	ch.Close()

	serverConn.Close()
}

func testBrotli() {
	start := time.Now()

	src := os.Args[2]
	inputBuffer, err := ioutil.ReadFile(src)
	if err != nil {
		panic(err)
	}

	log.Println("Read file in", time.Since(start))
	log.Println("Uncompressed size is", humanize.Bytes(uint64(len(inputBuffer))))
	start = time.Now()

	var decoded []byte

	for q := 0; q <= 9; q++ {
		params := enc.NewBrotliParams()
		params.SetQuality(q)

		encoded, err := enc.CompressBuffer(params, inputBuffer, make([]byte, 1))
		if err != nil {
			panic(err)
		}

		log.Println("Compressed (q=", q, ") to", humanize.Bytes(uint64(len(encoded))), "in", time.Since(start))
		start = time.Now()

		decoded, err = dec.DecompressBuffer(encoded, make([]byte, 1))
		if err != nil {
			panic(err)
		}

		log.Println("Decompressed in", time.Since(start))
		start = time.Now()
	}

	if !bytes.Equal(inputBuffer, decoded) {
		log.Println("Decoded output does not match original input")
		return
	}

	log.Println("Compared in", time.Since(start))
	start = time.Now()

	log.Println("Round-trip through brotli successful!")
}
