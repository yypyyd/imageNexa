package dola

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// upload.go implements Dola's reference-media upload chain:
// prepare_upload (STS) → ApplyImageUpload (AWS4-signed) → PUT TOS. The result
// is a StoreUri the chat transport references in attachment blocks.

const (
	prepareUploadPath = "/alice/resource/prepare_upload"
	imagexVersion     = "2018-08-01"
	maxUploadBytes    = 32 << 20
)

// UploadImage stores one reference image and returns its StoreUri.
func (c *Client) UploadImage(ctx context.Context, account Account, data []byte, contentType string) (string, error) {
	account = account.normalized()
	if account.Cookie == "" {
		return "", ErrAuth
	}
	if len(data) == 0 {
		return "", errors.New("dola: empty image")
	}
	if len(data) > maxUploadBytes {
		return "", fmt.Errorf("dola: image exceeds %d bytes", maxUploadBytes)
	}
	ext := ".png"
	switch strings.ToLower(strings.Split(contentType, ";")[0]) {
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	}

	sts, err := c.prepareUpload(ctx, account)
	if err != nil {
		return "", err
	}
	client, err := c.accountClient(ctx, account)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	storeURI, uploadHost, auth, err := applyImageUpload(ctx, client, sts, int64(len(data)), ext)
	if err != nil {
		return "", err
	}
	if err := putTOS(ctx, client, uploadHost, storeURI, auth, data); err != nil {
		return "", err
	}
	return storeURI, nil
}

type uploadSTS struct {
	ServiceID string
	Host      string
	AccessKey string
	SecretKey string
	Token     string
}

func (c *Client) prepareUpload(ctx context.Context, account Account) (uploadSTS, error) {
	body := map[string]any{"tenant_id": "5", "scene_id": "4", "resource_type": 2}
	var data []byte
	var status int
	var err error
	if c.usesProtocol() {
		if len(account.ProtocolQuery) == 0 {
			account, err = c.prepareProtocolAccount(ctx, account)
			if err != nil {
				return uploadSTS{}, err
			}
		}
		data, status, err = c.signedProtocolJSON(ctx, account, prepareUploadPath, commonQueryForTab(account, ""), body, false)
	} else {
		query := commonQuery(account)
		query.Set("device_id", "7655726059970627125")
		query.Set("pc_version", "3.23.10")
		query.Set("pkg_type", "release_version")
		query.Set("real_aid", dolaAID)
		query.Set("tea_uuid", "7655726485928068629")
		query.Set("web_id", "7655726485928068629")
		data, status, err = c.postJSON(ctx, account, prepareUploadPath, query, body,
			"application/json", "str, str", c.endpoint("/chat/"), 2<<20)
	}
	if err != nil {
		return uploadSTS{}, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return uploadSTS{}, ErrAuth
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			ServiceID  string `json:"service_id"`
			UploadHost string `json:"upload_host"`
			Auth       struct {
				AccessKey    string `json:"access_key"`
				SecretKey    string `json:"secret_key"`
				SessionToken string `json:"session_token"`
			} `json:"upload_auth_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return uploadSTS{}, fmt.Errorf("%w: invalid prepare_upload response", ErrTemporaryUpstream)
	}
	if envelope.Code != 0 {
		if isAuthCode(envelope.Code) {
			return uploadSTS{}, ErrAuth
		}
		return uploadSTS{}, fmt.Errorf("%w: prepare_upload code %d", ErrTemporaryUpstream, envelope.Code)
	}
	sts := uploadSTS{
		ServiceID: envelope.Data.ServiceID, Host: envelope.Data.UploadHost,
		AccessKey: envelope.Data.Auth.AccessKey, SecretKey: envelope.Data.Auth.SecretKey,
		Token: envelope.Data.Auth.SessionToken,
	}
	if sts.ServiceID == "" || sts.Host == "" || sts.AccessKey == "" {
		return uploadSTS{}, fmt.Errorf("%w: prepare_upload missing credentials", ErrTemporaryUpstream)
	}
	return sts, nil
}

// applyImageUpload calls the ImageX ApplyImageUpload endpoint with an AWS4
// signature and returns the store URI plus the TOS upload host and auth header.
func applyImageUpload(ctx context.Context, client *http.Client, sts uploadSTS, fileSize int64, ext string) (storeURI, tosHost, authHeader string, err error) {
	query := url.Values{}
	query.Set("Action", "ApplyImageUpload")
	query.Set("Version", imagexVersion)
	query.Set("ServiceId", sts.ServiceID)
	query.Set("FileSize", fmt.Sprintf("%d", fileSize))
	query.Set("FileExtension", ext)
	rawURL := "https://" + sts.Host + "/?" + query.Encode()

	sig := aws4Sign("GET", rawURL, sts.AccessKey, sts.SecretKey, sts.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", "", err
	}
	for key, value := range sig {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: apply upload: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var result struct {
		ResponseMetadata struct {
			Error *struct {
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"ResponseMetadata"`
		Result struct {
			UploadAddress struct {
				StoreInfos []struct {
					StoreURI string `json:"StoreUri"`
					Auth     string `json:"Auth"`
				} `json:"StoreInfos"`
				UploadHosts []string `json:"UploadHosts"`
			} `json:"UploadAddress"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", "", fmt.Errorf("%w: invalid apply upload response", ErrTemporaryUpstream)
	}
	if result.ResponseMetadata.Error != nil {
		return "", "", "", fmt.Errorf("%w: apply upload: %s", ErrTemporaryUpstream, result.ResponseMetadata.Error.Message)
	}
	if len(result.Result.UploadAddress.StoreInfos) == 0 || len(result.Result.UploadAddress.UploadHosts) == 0 {
		return "", "", "", fmt.Errorf("%w: apply upload returned no store", ErrTemporaryUpstream)
	}
	info := result.Result.UploadAddress.StoreInfos[0]
	return info.StoreURI, result.Result.UploadAddress.UploadHosts[0], info.Auth, nil
}

// putTOS uploads the bytes to the object store with the CRC32 the protocol
// requires.
func putTOS(ctx context.Context, client *http.Client, tosHost, storeURI, authHeader string, data []byte) error {
	crc := crc32.ChecksumIEEE(data)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://"+tosHost+"/"+storeURI, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("content-crc32", fmt.Sprintf("%x", crc))
	req.Header.Set("x-storage-u", "7655722254806270981")
	req.Header.Set("Content-Disposition", `attachment; filename="image.png"`)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: tos put: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var result struct {
		Success *int `json:"success"`
		Code    int  `json:"code"`
	}
	if err := json.Unmarshal(body, &result); err == nil {
		if result.Success != nil && *result.Success != 0 {
			return fmt.Errorf("%w: tos put rejected", ErrTemporaryUpstream)
		}
		if result.Code != 0 {
			return fmt.Errorf("%w: tos put code %d", ErrTemporaryUpstream, result.Code)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: tos put http %d", ErrTemporaryUpstream, resp.StatusCode)
	}
	return nil
}

// aws4Sign produces the AWS4-HMAC-SHA256 headers ImageX expects for the
// ApplyImageUpload GET.
func aws4Sign(method, rawURL, ak, sk, stsToken string) map[string]string {
	parsed, _ := url.Parse(rawURL)
	now := time.Now().UTC()
	date := now.Format("20060102T150405Z")
	dateShort := date[:8]

	params, _ := url.ParseQuery(parsed.RawQuery)
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		for _, value := range params[key] {
			parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(value))
		}
	}
	canonicalQuery := strings.Join(parts, "&")

	canonicalHeaders := "x-amz-date:" + date + "\nx-amz-security-token:" + stsToken + "\n"
	payloadHash := sha256Hex(nil)
	canonicalRequest := strings.Join([]string{
		method, parsed.Path, canonicalQuery, canonicalHeaders,
		"x-amz-date;x-amz-security-token", payloadHash,
	}, "\n")
	scope := dateShort + "/us-east-1/imagex/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", date, scope, sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+sk), dateShort)
	kRegion := hmacSHA256(kDate, "us-east-1")
	kService := hmacSHA256(kRegion, "imagex")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	return map[string]string{
		"Authorization":        "AWS4-HMAC-SHA256 Credential=" + ak + "/" + scope + ", SignedHeaders=x-amz-date;x-amz-security-token, Signature=" + signature,
		"x-amz-date":           date,
		"x-amz-security-token": stsToken,
	}
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
