package main

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
)

// awsChunkedReader decodes an AWS "aws-chunked" streaming upload body into the
// raw object bytes. The wire format is a sequence of:
//
//	<hex-size>;chunk-signature=<sig>\r\n<data>\r\n
//
// terminated by a zero-length chunk. Newer SDKs may append trailing headers
// (x-amz-trailer, e.g. checksums) after the final chunk; we read and discard
// them so the decoded stream is exactly the object payload.
//
// This lets the proxy re-sign uploads as UNSIGNED-PAYLOAD without needing the
// client's chunk signatures.
type awsChunkedReader struct {
	br        *bufio.Reader
	remaining int   // bytes left in the current chunk's data
	done      bool  // hit the terminating zero chunk
	err       error // sticky error
}

func newAWSChunkedReader(r io.Reader) *awsChunkedReader {
	return &awsChunkedReader{br: bufio.NewReaderSize(r, 64*1024)}
}

func (c *awsChunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.remaining == 0 {
		if c.done {
			return 0, io.EOF
		}
		if err := c.nextChunk(); err != nil {
			c.err = err
			return 0, err
		}
		if c.done {
			return 0, io.EOF
		}
	}
	n := len(p)
	if n > c.remaining {
		n = c.remaining
	}
	m, err := io.ReadFull(c.br, p[:n])
	c.remaining -= m
	if c.remaining == 0 && err == nil {
		// Consume the trailing CRLF after the chunk data.
		err = c.discardCRLF()
	}
	if err != nil {
		c.err = err
	}
	return m, err
}

// nextChunk reads a chunk header line and sets c.remaining / c.done.
func (c *awsChunkedReader) nextChunk() error {
	line, err := c.readLine()
	if err != nil {
		return err
	}
	// header is "<hexsize>[;chunk-signature=...]"
	sizeStr := line
	if i := strings.IndexByte(line, ';'); i >= 0 {
		sizeStr = line[:i]
	}
	sizeStr = strings.TrimSpace(sizeStr)
	size, err := strconv.ParseInt(sizeStr, 16, 64)
	if err != nil {
		return errors.New("aws-chunked: bad chunk size " + strconv.Quote(sizeStr))
	}
	if size == 0 {
		c.done = true
		c.drainTrailers()
		return nil
	}
	c.remaining = int(size)
	return nil
}

// drainTrailers reads any trailing header lines after the final chunk until a
// blank line or EOF. Errors here are non-fatal (best effort).
func (c *awsChunkedReader) drainTrailers() {
	for {
		line, err := c.readLine()
		if err != nil || line == "" {
			return
		}
	}
}

func (c *awsChunkedReader) discardCRLF() error {
	b, err := c.br.ReadByte()
	if err != nil {
		return err
	}
	if b == '\r' {
		if _, err = c.br.ReadByte(); err != nil {
			return err
		}
	}
	return nil
}

func (c *awsChunkedReader) readLine() (string, error) {
	line, err := c.br.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

// isAWSChunked reports whether the request body is aws-chunked encoded and
// should be decoded before forwarding.
func isAWSChunked(sha256Header, contentEncoding string) bool {
	if strings.HasPrefix(sha256Header, "STREAMING-") {
		return true
	}
	return strings.Contains(strings.ToLower(contentEncoding), "aws-chunked")
}
