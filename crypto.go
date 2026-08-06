package main

// Implements the same crypto contract as MedoroService in the WeddingScam backend:
//   - request:  DATA = AES-256-ECB(PKCS7) encrypted XML, KEY = RSA-PKCS1v15(gateway public,
//     AES key), SIGNATURE = RSA-PKCS1v15-SHA256(merchant private, XML).
//   - callback: same shape in the other direction — AES key encrypted with the merchant
//     public key, XML signed with the gateway private key.
// The mock plays the gateway, so it holds the gateway PRIVATE key and the merchant PUBLIC key.

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

type keyring struct {
	gatewayPriv *rsa.PrivateKey // decrypts incoming DATA, signs outgoing callbacks
	merchantPub *rsa.PublicKey  // verifies incoming SIGNATURE, encrypts outgoing AES key
}

// loadKeys reads the same two PEM files the backend uses. Both hold full private keys:
// the mock needs the gateway private half, and derives the merchant public half from the
// merchant private key. Keep these files byte-identical with the backend's copies.
func loadKeys(gatewayPrivPath, merchantPrivPath string) (*keyring, error) {
	gatewayPem, err := os.ReadFile(gatewayPrivPath)
	if err != nil {
		return nil, fmt.Errorf("read gateway key: %w", err)
	}
	gatewayPriv, err := parsePrivateKey(gatewayPem)
	if err != nil {
		return nil, fmt.Errorf("parse gateway key (must be the test private key, not the production public key): %w", err)
	}

	merchantPem, err := os.ReadFile(merchantPrivPath)
	if err != nil {
		return nil, fmt.Errorf("read merchant key: %w", err)
	}
	merchantPriv, err := parsePrivateKey(merchantPem)
	if err != nil {
		return nil, fmt.Errorf("parse merchant key: %w", err)
	}

	return &keyring{gatewayPriv: gatewayPriv, merchantPub: &merchantPriv.PublicKey}, nil
}

func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("PEM does not contain an RSA private key")
	}
	return key, nil
}

// decryptRequest opens the DATA/KEY pair sent by the backend's payment form.
func (k *keyring) decryptRequest(dataB64, keyB64 string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return nil, fmt.Errorf("DATA is not valid base64: %w", err)
	}
	encKey, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("KEY is not valid base64: %w", err)
	}
	aesKey, err := rsa.DecryptPKCS1v15(rand.Reader, k.gatewayPriv, encKey)
	if err != nil {
		return nil, fmt.Errorf("RSA decrypt of AES key failed (key mismatch — the backend's medoro_gateway.pem must be identical to the mock's copy): %w", err)
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("bad AES key: %w", err)
	}
	plain, err := ecbDecrypt(block, data)
	if err != nil {
		return nil, err
	}
	return pkcs7Unpad(plain, block.BlockSize())
}

// verifyRequestSignature checks SIGNATURE against the merchant public key.
func (k *keyring) verifyRequestSignature(xmlData []byte, sigB64 string) error {
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("SIGNATURE is not valid base64: %w", err)
	}
	hash := sha256.Sum256(xmlData)
	return rsa.VerifyPKCS1v15(k.merchantPub, crypto.SHA256, hash[:], sig)
}

// encryptCallback produces the DATA/KEY/SIGNATURE triple the backend expects on its
// callback endpoints.
func (k *keyring) encryptCallback(xmlData []byte) (dataB64, keyB64, sigB64 string, err error) {
	aesKey := make([]byte, 32)
	if _, err = rand.Read(aesKey); err != nil {
		return "", "", "", err
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", "", "", err
	}
	encrypted := ecbEncrypt(block, pkcs7Pad(xmlData, block.BlockSize()))

	encKey, err := rsa.EncryptPKCS1v15(rand.Reader, k.merchantPub, aesKey)
	if err != nil {
		return "", "", "", fmt.Errorf("RSA encrypt of AES key failed: %w", err)
	}
	hash := sha256.Sum256(xmlData)
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.gatewayPriv, crypto.SHA256, hash[:])
	if err != nil {
		return "", "", "", fmt.Errorf("signing failed: %w", err)
	}
	return base64.StdEncoding.EncodeToString(encrypted),
		base64.StdEncoding.EncodeToString(encKey),
		base64.StdEncoding.EncodeToString(sig),
		nil
}

func ecbEncrypt(block cipher.Block, plain []byte) []byte {
	out := make([]byte, len(plain))
	bs := block.BlockSize()
	for i := 0; i < len(plain); i += bs {
		block.Encrypt(out[i:i+bs], plain[i:i+bs])
	}
	return out
}

func ecbDecrypt(block cipher.Block, data []byte) ([]byte, error) {
	bs := block.BlockSize()
	if len(data) == 0 || len(data)%bs != 0 {
		return nil, errors.New("DATA length is not a multiple of the AES block size")
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += bs {
		block.Decrypt(out[i:i+bs], data[i:i+bs])
	}
	return out, nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty plaintext")
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > blockSize || pad > len(data) {
		return nil, errors.New("invalid PKCS7 padding")
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, errors.New("invalid PKCS7 padding")
		}
	}
	return data[:len(data)-pad], nil
}
