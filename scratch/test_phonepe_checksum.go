package main

import (
	"fmt"
	"passcheck/internal/vendors/phonepe"
)

func main() {
	payload := `{"merchantId":"M22Y46F064DZ6_2605251825", "merchantTransactionId":"MT7850590068188104", "merchantUserId":"MUID123", "amount":10000, "redirectUrl":"https://webhook.site/redirect-url", "redirectMode":"REDIRECT", "callbackUrl":"https://webhook.site/callback-url", "mobileNumber":"9999999999", "paymentInstrument":{"type":"PAY_PAGE"}}`
	endpoint := "/pg/v1/pay"
	saltKey := "ODljYWU1ZTEtYjEzNi00NjRjLTk5OGUtZGI3YjY0NzMwMmM5"
	saltIndex := "1"

	checksum := phonepe.GenerateChecksum(payload, endpoint, saltKey, saltIndex)
	fmt.Println("Generated Checksum:", checksum)
}
