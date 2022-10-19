package virtual_fido

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"log"
	"math/big"
	"time"
)

type ClientAction uint8

type ClientActionRequestParams struct {
	RelyingParty string
	UserName     string
}

const (
	ClientActionU2FRegister        ClientAction = 0
	ClientActionU2FAuthenticate    ClientAction = 1
	ClientActionFIDOMakeCredential ClientAction = 2
	ClientActionFIDOGetAssertion   ClientAction = 3
)

var clientLogger *log.Logger = newLogger("[CLIENT] ", false)

type ClientRequestApprover interface {
	ApproveClientAction(action ClientAction, params ClientActionRequestParams) bool
}

type ClientDataSaver interface {
	SaveData(data []byte)
	RetrieveData() []byte
	Passphrase() string
}

type FIDOClient interface {
	NewCredentialSource(relyingParty PublicKeyCredentialRpEntity, user PublicKeyCrendentialUserEntity) *CredentialSource
	GetAssertionSource(relyingPartyID string, allowList []PublicKeyCredentialDescriptor) *CredentialSource

	SealingEncryptionKey() []byte
	NewPrivateKey() *ecdsa.PrivateKey
	NewAuthenticationCounterId() uint32
	CreateAttestationCertificiate(privateKey *ecdsa.PrivateKey) []byte

	ApproveAccountCreation(relyingParty string) bool
	ApproveAccountLogin(credentialSource *CredentialSource) bool
	ApproveU2FRegistration(keyHandle *KeyHandle) bool
	ApproveU2FAuthentication(keyHandle *KeyHandle) bool
}

type DefaultFIDOClient struct {
	deviceEncryptionKey   []byte
	certificateAuthority  *x509.Certificate
	certPrivateKey        *ecdsa.PrivateKey
	authenticationCounter uint32
	vault                 *IdentityVault
	requestApprover       ClientRequestApprover
	dataSaver             ClientDataSaver
}

func NewClient(
	attestationCertificate []byte,
	certificatePrivateKey *ecdsa.PrivateKey,
	secretEncryptionKey [32]byte,
	requestApprover ClientRequestApprover,
	dataSaver ClientDataSaver) *DefaultFIDOClient {
	authorityCert, err := x509.ParseCertificate(attestationCertificate)
	checkErr(err, "Could not parse authority CA cert")
	client := &DefaultFIDOClient{
		deviceEncryptionKey:   secretEncryptionKey[:],
		certificateAuthority:  authorityCert,
		certPrivateKey:        certificatePrivateKey,
		authenticationCounter: 1,
		vault:                 NewIdentityVault(),
		requestApprover:       requestApprover,
		dataSaver:             dataSaver,
	}
	client.loadData()
	return client
}

func (client *DefaultFIDOClient) NewCredentialSource(relyingParty PublicKeyCredentialRpEntity, user PublicKeyCrendentialUserEntity) *CredentialSource {
	newSource := client.vault.NewIdentity(relyingParty, user)
	client.saveData()
	return newSource
}

func (client *DefaultFIDOClient) GetAssertionSource(relyingPartyID string, allowList []PublicKeyCredentialDescriptor) *CredentialSource {
	sources := client.vault.GetMatchingCredentialSources(relyingPartyID, allowList)
	if len(sources) == 0 {
		clientLogger.Printf("ERROR: No Credentials\n\n")
		return nil
	}

	// TODO: Allow user to choose credential source
	credentialSource := sources[0]
	credentialSource.SignatureCounter++
	client.saveData()
	return credentialSource
}

func (client DefaultFIDOClient) ApproveAccountCreation(relyingParty string) bool {
	params := ClientActionRequestParams{
		RelyingParty: relyingParty,
	}
	return client.requestApprover.ApproveClientAction(ClientActionFIDOMakeCredential, params)
}

func (client DefaultFIDOClient) ApproveAccountLogin(credentialSource *CredentialSource) bool {
	params := ClientActionRequestParams{
		RelyingParty: credentialSource.RelyingParty.Name,
		UserName:     credentialSource.User.Name,
	}
	return client.requestApprover.ApproveClientAction(ClientActionFIDOGetAssertion, params)
}

// -----------------------------
// U2F Methods
// -----------------------------

func (client DefaultFIDOClient) SealingEncryptionKey() []byte {
	return client.deviceEncryptionKey
}

func (client *DefaultFIDOClient) NewPrivateKey() *ecdsa.PrivateKey {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	checkErr(err, "Could not generate private key")
	return privateKey
}

func (client *DefaultFIDOClient) NewAuthenticationCounterId() uint32 {
	num := client.authenticationCounter
	client.authenticationCounter++
	return num
}

func (client *DefaultFIDOClient) CreateAttestationCertificiate(privateKey *ecdsa.PrivateKey) []byte {
	// TODO: Fill in fields like SerialNumber and SubjectKeyIdentifier
	templateCert := &x509.Certificate{
		SerialNumber: big.NewInt(0),
		Subject: pkix.Name{
			Organization: []string{"Self-Signed Virtual FIDO"},
			Country:      []string{"US"},
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().AddDate(10, 0, 0),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature,
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, templateCert, client.certificateAuthority, &privateKey.PublicKey, client.certPrivateKey)
	checkErr(err, "Could not generate attestation certificate")
	return certBytes
}

func (client DefaultFIDOClient) ApproveU2FRegistration(keyHandle *KeyHandle) bool {
	params := ClientActionRequestParams{}
	return client.requestApprover.ApproveClientAction(ClientActionU2FRegister, params)
}

func (client DefaultFIDOClient) ApproveU2FAuthentication(keyHandle *KeyHandle) bool {
	params := ClientActionRequestParams{}
	return client.requestApprover.ApproveClientAction(ClientActionU2FAuthenticate, params)
}

func (client *DefaultFIDOClient) exportData(passphrase string) []byte {
	privKeyBytes, err := x509.MarshalECPrivateKey(client.certPrivateKey)
	checkErr(err, "Could not marshal private key")
	identityData := client.vault.Export()
	state := FIDODeviceConfig{
		EncryptionKey:          client.deviceEncryptionKey,
		AttestationCertificate: client.certificateAuthority.Raw,
		AttestationPrivateKey:  privKeyBytes,
		AuthenticationCounter:  client.authenticationCounter,
		Sources:                identityData,
	}
	savedBytes, err := EncryptWithPassphrase(state, passphrase)
	checkErr(err, "Could not encode saved state")
	return savedBytes
}

func (client *DefaultFIDOClient) importData(data []byte, passphrase string) error {
	state, err := DecryptWithPassphrase(data, passphrase)
	checkErr(err, "Could not decrypt vault data")
	cert, err := x509.ParseCertificate(state.AttestationCertificate)
	checkErr(err, "Could not parse x509 cert")
	privateKey, err := x509.ParseECPrivateKey(state.AttestationPrivateKey)
	checkErr(err, "Could not parse private key")
	client.deviceEncryptionKey = state.EncryptionKey
	client.certificateAuthority = cert
	client.certPrivateKey = privateKey
	client.authenticationCounter = state.AuthenticationCounter
	client.vault = NewIdentityVault()
	client.vault.Import(state.Sources)
	return nil
}

func (client *DefaultFIDOClient) saveData() {
	data := client.exportData(client.dataSaver.Passphrase())
	client.dataSaver.SaveData(data)
}

func (client *DefaultFIDOClient) loadData() {
	data := client.dataSaver.RetrieveData()
	if data != nil {
		client.importData(data, client.dataSaver.Passphrase())
	}
}

func (client *DefaultFIDOClient) Identities() []CredentialSource {
	sources := make([]CredentialSource, 0)
	for _, source := range client.vault.CredentialSources {
		sources = append(sources, *source)
	}
	return sources
}

func (client *DefaultFIDOClient) DeleteIdentity(id []byte) bool {
	success := client.vault.DeleteIdentity(id)
	if success {
		client.saveData()
	}
	return success
}