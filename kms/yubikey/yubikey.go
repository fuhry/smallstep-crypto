//go:build cgo
// +build cgo

package yubikey

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/hex"
	"net/url"
	"strings"

	"github.com/go-piv/piv-go/piv"
	"github.com/pkg/errors"
	"go.step.sm/crypto/kms/apiv1"
	"go.step.sm/crypto/kms/uri"
)

// Scheme is the scheme used in uris.
const Scheme = "yubikey"

// YubiKey implements the KMS interface on a YubiKey.
type YubiKey struct {
	yk            pivKey
	pin           string
	managementKey [24]byte
}

type pivKey interface {
	Certificate(slot piv.Slot) (*x509.Certificate, error)
	SetCertificate(key [24]byte, slot piv.Slot, cert *x509.Certificate) error
	GenerateKey(key [24]byte, slot piv.Slot, opts piv.Key) (crypto.PublicKey, error)
	PrivateKey(slot piv.Slot, public crypto.PublicKey, auth piv.KeyAuth) (crypto.PrivateKey, error)
	Attest(slot piv.Slot) (*x509.Certificate, error)
	Close() error
}

var pivCards = piv.Cards

var pivOpen = func(card string) (pivKey, error) {
	return piv.Open(card)
}

// New initializes a new YubiKey.
// TODO(mariano): only one card is currently supported.
func New(ctx context.Context, opts apiv1.Options) (*YubiKey, error) {
	pin := "123456"
	managementKey := piv.DefaultManagementKey

	if opts.URI != "" {
		u, err := uri.ParseWithScheme(Scheme, opts.URI)
		if err != nil {
			return nil, err
		}
		if v := u.Pin(); v != "" {
			opts.Pin = v
		}
		if v := u.Get("management-key"); v != "" {
			opts.ManagementKey = v
		}
	}

	// Deprecated way to set configuration parameters.
	if opts.ManagementKey != "" {
		b, err := hex.DecodeString(opts.ManagementKey)
		if err != nil {
			return nil, errors.Wrap(err, "error decoding managementKey")
		}
		if len(b) != 24 {
			return nil, errors.New("invalid managementKey: length is not 24 bytes")
		}
		copy(managementKey[:], b[:24])
	}

	if opts.Pin != "" {
		pin = opts.Pin
	}

	cards, err := pivCards()
	if err != nil {
		return nil, err
	}
	if len(cards) == 0 {
		return nil, errors.New("error detecting yubikey: try removing and reconnecting the device")
	}

	yk, err := pivOpen(cards[0])
	if err != nil {
		return nil, errors.Wrap(err, "error opening yubikey")
	}

	return &YubiKey{
		yk:            yk,
		pin:           pin,
		managementKey: managementKey,
	}, nil
}

func init() {
	apiv1.Register(apiv1.YubiKey, func(ctx context.Context, opts apiv1.Options) (apiv1.KeyManager, error) {
		return New(ctx, opts)
	})
}

// LoadCertificate implements kms.CertificateManager and loads a certificate
// from the YubiKey.
func (k *YubiKey) LoadCertificate(req *apiv1.LoadCertificateRequest) (*x509.Certificate, error) {
	slot, err := getSlot(req.Name)
	if err != nil {
		return nil, err
	}

	cert, err := k.yk.Certificate(slot)
	if err != nil {
		return nil, errors.Wrap(err, "error retrieving certificate")
	}

	return cert, nil
}

// StoreCertificate implements kms.CertificateManager and stores a certificate
// in the YubiKey.
func (k *YubiKey) StoreCertificate(req *apiv1.StoreCertificateRequest) error {
	if req.Certificate == nil {
		return errors.New("storeCertificateRequest 'Certificate' cannot be nil")
	}

	slot, err := getSlot(req.Name)
	if err != nil {
		return err
	}

	err = k.yk.SetCertificate(k.managementKey, slot, req.Certificate)
	if err != nil {
		return errors.Wrap(err, "error storing certificate")
	}

	return nil
}

// GetPublicKey returns the public key present in the YubiKey signature slot.
func (k *YubiKey) GetPublicKey(req *apiv1.GetPublicKeyRequest) (crypto.PublicKey, error) {
	slot, err := getSlot(req.Name)
	if err != nil {
		return nil, err
	}

	pub, err := k.getPublicKey(slot)
	if err != nil {
		return nil, err
	}

	return pub, nil
}

// CreateKey generates a new key in the YubiKey and returns the public key.
func (k *YubiKey) CreateKey(req *apiv1.CreateKeyRequest) (*apiv1.CreateKeyResponse, error) {
	alg, err := getSignatureAlgorithm(req.SignatureAlgorithm, req.Bits)
	if err != nil {
		return nil, err
	}
	slot, name, err := getSlotAndName(req.Name)
	if err != nil {
		return nil, err
	}
	pinPolicy, touchPolicy := getPolicies(req)

	pub, err := k.yk.GenerateKey(k.managementKey, slot, piv.Key{
		Algorithm:   alg,
		PINPolicy:   pinPolicy,
		TouchPolicy: touchPolicy,
	})
	if err != nil {
		return nil, errors.Wrap(err, "error generating key")
	}
	return &apiv1.CreateKeyResponse{
		Name:      name,
		PublicKey: pub,
		CreateSignerRequest: apiv1.CreateSignerRequest{
			SigningKey: name,
		},
	}, nil
}

// CreateSigner creates a signer using the key present in the YubiKey signature
// slot.
func (k *YubiKey) CreateSigner(req *apiv1.CreateSignerRequest) (crypto.Signer, error) {
	slot, err := getSlot(req.SigningKey)
	if err != nil {
		return nil, err
	}

	pin := k.pin
	if pin == "" {
		// Attempt to get the pin from the uri
		if u, err := uri.ParseWithScheme(Scheme, req.SigningKey); err == nil {
			pin = u.Pin()
		}
	}

	pub, err := k.getPublicKey(slot)
	if err != nil {
		return nil, err
	}

	priv, err := k.yk.PrivateKey(slot, pub, piv.KeyAuth{
		PIN:       pin,
		PINPolicy: piv.PINPolicyAlways,
	})
	if err != nil {
		return nil, errors.Wrap(err, "error retrieving private key")
	}

	signer, ok := priv.(crypto.Signer)
	if !ok {
		return nil, errors.New("private key is not a crypto.Signer")
	}
	return signer, nil
}

// CreateAttestation creates an attestation certificate from a YubiKey slot.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a later
// release.
func (k *YubiKey) CreateAttestation(req *apiv1.CreateAttestationRequest) (*apiv1.CreateAttestationResponse, error) {
	slot, err := getSlot(req.Name)
	if err != nil {
		return nil, err
	}

	cert, err := k.yk.Attest(slot)
	if err != nil {
		return nil, errors.Wrap(err, "error attesting slot")
	}

	intermediate, err := k.yk.Certificate(slotAttestation)
	if err != nil {
		return nil, errors.Wrap(err, "error retrieving attestation certificate")
	}

	return &apiv1.CreateAttestationResponse{
		Certificate:      cert,
		CertificateChain: []*x509.Certificate{intermediate},
		PublicKey:        cert.PublicKey,
	}, nil
}

// Close releases the connection to the YubiKey.
func (k *YubiKey) Close() error {
	return errors.Wrap(k.yk.Close(), "error closing yubikey")
}

// getPublicKey returns the public key on a slot. First it attempts to do
// attestation to get a certificate with the public key in it, if this succeeds
// means that the key was generated in the device. If not we'll try to get the
// key from a stored certificate in the same slot.
func (k *YubiKey) getPublicKey(slot piv.Slot) (crypto.PublicKey, error) {
	cert, err := k.yk.Attest(slot)
	if err != nil {
		if cert, err = k.yk.Certificate(slot); err != nil {
			return nil, errors.Wrap(err, "error retrieving public key")
		}
	}
	return cert.PublicKey, nil
}

// signatureAlgorithmMapping is a mapping between the step signature algorithm,
// and bits for RSA keys, with yubikey ones.
var signatureAlgorithmMapping = map[apiv1.SignatureAlgorithm]interface{}{
	apiv1.UnspecifiedSignAlgorithm: piv.AlgorithmEC256,
	apiv1.SHA256WithRSA: map[int]piv.Algorithm{
		0:    piv.AlgorithmRSA2048,
		1024: piv.AlgorithmRSA1024,
		2048: piv.AlgorithmRSA2048,
	},
	apiv1.SHA512WithRSA: map[int]piv.Algorithm{
		0:    piv.AlgorithmRSA2048,
		1024: piv.AlgorithmRSA1024,
		2048: piv.AlgorithmRSA2048,
	},
	apiv1.SHA256WithRSAPSS: map[int]piv.Algorithm{
		0:    piv.AlgorithmRSA2048,
		1024: piv.AlgorithmRSA1024,
		2048: piv.AlgorithmRSA2048,
	},
	apiv1.SHA512WithRSAPSS: map[int]piv.Algorithm{
		0:    piv.AlgorithmRSA2048,
		1024: piv.AlgorithmRSA1024,
		2048: piv.AlgorithmRSA2048,
	},
	apiv1.ECDSAWithSHA256: piv.AlgorithmEC256,
	apiv1.ECDSAWithSHA384: piv.AlgorithmEC384,
	apiv1.PureEd25519:     piv.AlgorithmEd25519,
}

func getSignatureAlgorithm(alg apiv1.SignatureAlgorithm, bits int) (piv.Algorithm, error) {
	v, ok := signatureAlgorithmMapping[alg]
	if !ok {
		return 0, errors.Errorf("YubiKey does not support signature algorithm '%s'", alg)
	}

	switch v := v.(type) {
	case piv.Algorithm:
		return v, nil
	case map[int]piv.Algorithm:
		signatureAlgorithm, ok := v[bits]
		if !ok {
			return 0, errors.Errorf("YubiKey does not support signature algorithm '%s' with '%d' bits", alg, bits)
		}
		return signatureAlgorithm, nil
	default:
		return 0, errors.Errorf("unexpected error: this should not happen")
	}
}

var slotAttestation = piv.Slot{Key: 0xf9, Object: 0x5fff01}

var slotMapping = map[string]piv.Slot{
	"9a": piv.SlotAuthentication,
	"9c": piv.SlotSignature,
	"9e": piv.SlotCardAuthentication,
	"9d": piv.SlotKeyManagement,
	"82": {Key: 0x82, Object: 0x5FC10D},
	"83": {Key: 0x83, Object: 0x5FC10E},
	"84": {Key: 0x84, Object: 0x5FC10F},
	"85": {Key: 0x85, Object: 0x5FC110},
	"86": {Key: 0x86, Object: 0x5FC111},
	"87": {Key: 0x87, Object: 0x5FC112},
	"88": {Key: 0x88, Object: 0x5FC113},
	"89": {Key: 0x89, Object: 0x5FC114},
	"8a": {Key: 0x8a, Object: 0x5FC115},
	"8b": {Key: 0x8b, Object: 0x5FC116},
	"8c": {Key: 0x8c, Object: 0x5FC117},
	"8d": {Key: 0x8d, Object: 0x5FC118},
	"8e": {Key: 0x8e, Object: 0x5FC119},
	"8f": {Key: 0x8f, Object: 0x5FC11A},
	"90": {Key: 0x90, Object: 0x5FC11B},
	"91": {Key: 0x91, Object: 0x5FC11C},
	"92": {Key: 0x92, Object: 0x5FC11D},
	"93": {Key: 0x93, Object: 0x5FC11E},
	"94": {Key: 0x94, Object: 0x5FC11F},
	"95": {Key: 0x95, Object: 0x5FC120},
}

func getSlot(name string) (piv.Slot, error) {
	slot, _, err := getSlotAndName(name)
	return slot, err
}

func getSlotAndName(name string) (piv.Slot, string, error) {
	if name == "" {
		return piv.SlotSignature, "yubikey:slot-id=9c", nil
	}

	var slotID string
	name = strings.ToLower(name)
	if strings.HasPrefix(name, "yubikey:") {
		u, err := url.Parse(name)
		if err != nil {
			return piv.Slot{}, "", errors.Wrapf(err, "error parsing '%s'", name)
		}
		v, err := url.ParseQuery(u.Opaque)
		if err != nil {
			return piv.Slot{}, "", errors.Wrapf(err, "error parsing '%s'", name)
		}
		if slotID = v.Get("slot-id"); slotID == "" {
			return piv.Slot{}, "", errors.Errorf("error parsing '%s': slot-id is missing", name)
		}
	} else {
		slotID = name
	}

	s, ok := slotMapping[slotID]
	if !ok {
		return piv.Slot{}, "", errors.Errorf("unsupported slot-id '%s'", name)
	}

	name = "yubikey:slot-id=" + url.QueryEscape(slotID)
	return s, name, nil
}

// getPolicies returns the pin and touch policies from the request. If they are
// not set the defaults are piv.PINPolicyAlways and piv.TouchPolicyNever.
func getPolicies(req *apiv1.CreateKeyRequest) (piv.PINPolicy, piv.TouchPolicy) {
	pin := piv.PINPolicy(req.PINPolicy)
	touch := piv.TouchPolicy(req.TouchPolicy)
	if pin == 0 {
		pin = piv.PINPolicyAlways
	}
	if touch == 0 {
		touch = piv.TouchPolicyNever
	}
	return pin, touch
}
