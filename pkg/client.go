// Copyright 2016 David Lazar. All rights reserved.
// Use of this source code is governed by the GNU AGPL
// license that can be found in the LICENSE file.

package pkg

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"

	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/nacl/box"

	"vuvuzela.io/alpenhorn/edhttp"
	"vuvuzela.io/alpenhorn/errors"
	"vuvuzela.io/crypto/bls"
	"vuvuzela.io/crypto/ibe"
)

// A Client connects to a PKG server to extract private keys.
// Before a client can extract keys, it must register the username
// and login key with the PKG server. The client must then verify
// ownership of the username, unless the PKG server is running in
// first-come-first-serve mode.
type Client struct {
	PublicServerConfig

	// Username is identity in Identity-Based Encryption.
	Username string

	// LoginKey is used to authenticate to the PKG server.
	LoginKey ed25519.PrivateKey

	// UserLongTermKey is the user's long-term signing key. The
	// PKG server attests to this key during extraction. JSON
	// ignores this field since it does not need to be persisted.
	UserLongTermKey ed25519.PublicKey `json:"-"`

	once       sync.Once
	httpClient *edhttp.Client
}

// Register attempts to register the client's username and login key
// with the PKG server. It only needs to be called once per PKG server.
func (c *Client) Register() error {
	loginPublicKey := c.LoginKey.Public()
	args := &registerArgs{
		Username: c.Username,
		LoginKey: loginPublicKey.(ed25519.PublicKey),
	}

	var reply string
	err := c.do("register", args, &reply)
	if err != nil {
		return err
	}
	return nil
}

// Verify is used to verify ownership of a username (email address)
// when the PKG is not in first-come-first-serve mode.
func (c *Client) Verify(token []byte) error {
	args := &verifyArgs{
		Username: c.Username,
		Token:    token,
	}
	args.Sign(c.LoginKey)

	var reply string
	err := c.do("verify", args, &reply)
	if err != nil {
		return err
	}
	return nil
}

func (c *Client) CheckStatus() error {
	args := &statusArgs{
		Username:         c.Username,
		ServerSigningKey: c.PublicServerConfig.Key,
	}
	rand.Read(args.Message[:])
	args.Signature = ed25519.Sign(c.LoginKey, args.msg())

	var reply statusReply
	err := c.do("status", args, &reply)
	if err != nil {
		return err
	}
	return nil
}

type ExtractResult struct {
	PrivateKey  *ibe.IdentityPrivateKey
	IdentitySig bls.Signature
}

// Extract obtains the user's IBE private key for the given round from the PKG.
func (c *Client) Extract(round uint32) (*ExtractResult, error) {
	myPub, myPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		panic("box.GenerateKey: " + err.Error())
	}

	args := &extractArgs{
		Round:            round,
		Username:         c.Username,
		ReturnKey:        myPub,
		UserLongTermKey:  c.UserLongTermKey,
		ServerSigningKey: c.PublicServerConfig.Key,
	}
	args.Sign(c.LoginKey)

	reply := new(extractReply)
	err = c.do("extract", args, reply)
	if err != nil {
		return nil, err
	}

	if reply.Round != round {
		return nil, errors.New("expected reply for round %d, but got %d", round, reply.Round)
	}
	if reply.Username != c.Username {
		return nil, errors.New("expected reply for username %q, but got %q", c.Username, reply.Username)
	}
	if l := len(reply.EncryptedPrivateKey); l < 32 {
		return nil, errors.New("unexpectedly short ciphertext (%d bytes)", l)
	}
	if !reply.Verify(c.PublicServerConfig.Key) {
		return nil, errors.New("invalid signature")
	}

	theirPub := new([32]byte)
	copy(theirPub[:], reply.EncryptedPrivateKey[0:32])
	ctxt := reply.EncryptedPrivateKey[32:]
	msg, ok := box.Open(nil, ctxt, new([24]byte), theirPub, myPriv)
	if !ok {
		return nil, errors.New("box authentication failed")
	}

	ibeKey := new(ibe.IdentityPrivateKey)
	if err := ibeKey.UnmarshalBinary(msg); err != nil {
		return nil, errors.Wrap(err, "unmarshalling ibe identity key")
	}

	return &ExtractResult{
		PrivateKey:  ibeKey,
		IdentitySig: reply.IdentitySig,
	}, nil
}

func (c *Client) do(path string, args, reply interface{}) error {
	c.once.Do(func() {
		c.httpClient = &edhttp.Client{}
	})

	req := &pkgRequest{
		PublicServerConfig: c.PublicServerConfig,
		Path:               path,
		Args:               args,
		Reply:              reply,
		Client:             c.httpClient,
		TweakRequest: func(req *http.Request) {
			req.Close = true
		},
	}

	return req.Do()
}

type pkgRequest struct {
	PublicServerConfig

	Path   string
	Args   interface{}
	Reply  interface{}
	Client *edhttp.Client

	TweakRequest func(*http.Request)
}

func (req *pkgRequest) Do() error {
	buf := new(bytes.Buffer)
	if err := json.NewEncoder(buf).Encode(req.Args); err != nil {
		return errors.Wrap(err, "json.Encode")
	}

	url := fmt.Sprintf("https://%s/%s", req.PublicServerConfig.Address, req.Path)
	httpReq, err := http.NewRequest("POST", url, buf)
	if err != nil {
		return err
	}
	if req.TweakRequest != nil {
		req.TweakRequest(httpReq)
	}

	resp, err := req.Client.Do(req.PublicServerConfig.Key, httpReq)
	if err != nil {
		return errors.Wrap(err, "error making PKG request %q", url)
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "reading http response body")
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, req.Reply); err != nil {
			return errors.Wrap(err, "json.Unmarshal")
		}
		return nil
	} else {
		var pkgErr Error
		if err := json.Unmarshal(body, &pkgErr); err != nil {
			return errors.New(
				"error response (%s) with unparseable body: %q",
				resp.Status, body,
			)
		}
		return pkgErr
	}
}