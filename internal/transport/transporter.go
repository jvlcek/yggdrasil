package transport

import "crypto/tls"

type RxHandlerFunc func(addr string, metadata map[string]interface{}, data []byte) error

// Transporter is an interface representing the ability to send and receive
// data. It abstracts away the concrete implementation, leaving that up to the
// implementing type.
type Transporter interface {
	// Connect begins listening over specific network connections and receiving
	// data.
	Connect() error

	// Disconnect disconnects the transport, performing any graceful shutdown
	// necessary.
	Disconnect(quiesce uint)

	// Tx sends a message to the given address, using metadata and data
	// according to the specific nature of the transport.Transporter
	// implementation.
	Tx(addr string, metadata map[string]string, data []byte) (responseCode int, responseMetadata map[string]string, responseData []byte, err error)

	// SetRxHandler stores a reference to f, which is then called whenever data
	// is received over the network.
	SetRxHandler(f RxHandlerFunc) error

	// ReloadTLSConfig forces the transport to replace its TLS configuration
	// with tlsConfig.
	ReloadTLSConfig(tlsConfig *tls.Config) error
}