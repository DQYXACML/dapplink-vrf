package ethereumcli

import (
	"github.com/ethereum/go-ethereum/ethclient"
	"golang.org/x/net/context"
	"time"
)

func EthClientWithTimeout(ctx context.Context, url string) (*ethclient.Client, error) {
	ctxt, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return ethclient.DialContext(ctxt, url)
}
