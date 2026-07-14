// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go.mau.fi/whatsmeow/util/cbcutil"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func prepareEncryptedDownload(t *testing.T, plaintext []byte) (*Client, string, []byte, []byte) {
	t.Helper()
	mediaKey := bytes.Repeat([]byte{0x42}, 32)
	iv, cipherKey, macKey, _ := getMediaKeys(mediaKey, MediaImage)
	ciphertext, err := cbcutil.Encrypt(cipherKey, iv, plaintext)
	if err != nil {
		t.Fatalf("failed to encrypt fixture: %v", err)
	}
	mac := hmac.New(sha256.New, macKey)
	_, _ = mac.Write(iv)
	_, _ = mac.Write(ciphertext)
	payload := append(append([]byte{}, ciphertext...), mac.Sum(nil)[:mediaHMACLength]...)
	encHash := sha256.Sum256(payload)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(payload)
	}))
	t.Cleanup(server.Close)
	client := &Client{mediaHTTP: server.Client(), Log: waLog.Noop}
	return client, server.URL, mediaKey, encHash[:]
}

func TestDownloadAndDecryptAllowsMissingPlaintextHash(t *testing.T) {
	plaintext := []byte("encrypted media without a plaintext hash")
	client, downloadURL, mediaKey, encHash := prepareEncryptedDownload(t, plaintext)

	got, err := client.downloadAndDecrypt(context.Background(), downloadURL, mediaKey, MediaImage, encHash, nil)
	if err != nil {
		t.Fatalf("downloadAndDecrypt() error = %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("downloadAndDecrypt() = %q, want %q", got, plaintext)
	}
}

func TestDownloadAndDecryptAcceptsCorrectPlaintextHash(t *testing.T) {
	plaintext := []byte("encrypted media with a valid plaintext hash")
	client, downloadURL, mediaKey, encHash := prepareEncryptedDownload(t, plaintext)
	plainHash := sha256.Sum256(plaintext)

	got, err := client.downloadAndDecrypt(context.Background(), downloadURL, mediaKey, MediaImage, encHash, plainHash[:])
	if err != nil {
		t.Fatalf("downloadAndDecrypt() error = %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("downloadAndDecrypt() = %q, want %q", got, plaintext)
	}
}

func TestDownloadAndDecryptStillRejectsWrongPlaintextHash(t *testing.T) {
	client, downloadURL, mediaKey, encHash := prepareEncryptedDownload(t, []byte("encrypted media"))
	wrongHash := bytes.Repeat([]byte{0x99}, sha256.Size)

	_, err := client.downloadAndDecrypt(context.Background(), downloadURL, mediaKey, MediaImage, encHash, wrongHash)
	if !errors.Is(err, ErrInvalidMediaSHA256) {
		t.Fatalf("downloadAndDecrypt() error = %v, want %v", err, ErrInvalidMediaSHA256)
	}
}

func TestDownloadAndDecryptToFileAllowsMissingPlaintextHash(t *testing.T) {
	plaintext := []byte("encrypted file without a plaintext hash")
	client, downloadURL, mediaKey, encHash := prepareEncryptedDownload(t, plaintext)
	file, err := os.CreateTemp(t.TempDir(), "download-*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer file.Close()

	err = client.downloadAndDecryptToFile(context.Background(), downloadURL, mediaKey, MediaImage, encHash, nil, file)
	if err != nil {
		t.Fatalf("downloadAndDecryptToFile() error = %v", err)
	}
	if _, err = file.Seek(0, 0); err != nil {
		t.Fatalf("failed to seek output: %v", err)
	}
	got, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatalf("failed to read output: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("downloaded file = %q, want %q", got, plaintext)
	}
}

func TestDownloadAndDecryptToFileAcceptsCorrectPlaintextHash(t *testing.T) {
	plaintext := []byte("encrypted file with a valid plaintext hash")
	client, downloadURL, mediaKey, encHash := prepareEncryptedDownload(t, plaintext)
	plainHash := sha256.Sum256(plaintext)
	file, err := os.CreateTemp(t.TempDir(), "download-*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer file.Close()

	err = client.downloadAndDecryptToFile(context.Background(), downloadURL, mediaKey, MediaImage, encHash, plainHash[:], file)
	if err != nil {
		t.Fatalf("downloadAndDecryptToFile() error = %v", err)
	}
	got, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatalf("failed to read output: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("downloaded file = %q, want %q", got, plaintext)
	}
}
