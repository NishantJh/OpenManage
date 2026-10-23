// Command radius-probe sends one PAP Access-Request and prints accept, reject,
// challenge or timeout. The OpenWrt lab copies it into the router container so the request
// comes from the enrolled AP's address. It is a test tool, not part of the app.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2869"
)

func main() {
	server := flag.String("server", "", "host:port of the RADIUS server")
	secret := flag.String("secret", "", "shared secret")
	user := flag.String("user", "", "User-Name")
	password := flag.String("password", "", "User-Password")
	noMA := flag.Bool("no-message-authenticator", false, "omit Message-Authenticator")
	eap := flag.Bool("eap", false, "add an EAP-Response/Identity")
	repeat := flag.Int("repeat", 1, "send the same request this many times")
	station := flag.String("station", "", "Calling-Station-Id (the client MAC address)")
	flag.Parse()

	request := radius.New(radius.CodeAccessRequest, []byte(*secret))
	if !*noMA {
		request.Add(rfc2869.MessageAuthenticator_Type, make([]byte, 16))
	}
	rfc2865.UserName_SetString(request, *user)
	rfc2865.UserPassword_SetString(request, *password)
	if *station != "" {
		rfc2865.CallingStationID_SetString(request, *station)
	}
	if *eap {
		request.Add(rfc2869.EAPMessage_Type, []byte{2, 0, 0, 5, 1})
	}
	raw, err := request.Encode()
	if err != nil {
		fail(err)
	}
	if !*noMA {
		mac := hmac.New(md5.New, []byte(*secret))
		mac.Write(raw)
		copy(raw[22:38], mac.Sum(nil))
	}

	conn, err := net.Dial("udp", *server)
	if err != nil {
		fail(err)
	}
	defer conn.Close()
	var last []byte
	for i := 0; i < *repeat; i++ {
		if _, err := conn.Write(raw); err != nil {
			fail(err)
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, radius.MaxPacketLength)
		n, err := conn.Read(buf)
		if err != nil {
			fmt.Println("timeout")
			return
		}
		reply := buf[:n]
		if !radius.IsAuthenticResponse(reply, raw, []byte(*secret)) || !replyMessageAuthenticatorOK(reply, raw, []byte(*secret)) {
			fail(fmt.Errorf("reply failed authentication"))
		}
		if last != nil && !bytes.Equal(last, reply) {
			fail(fmt.Errorf("retransmission got a different reply"))
		}
		last = reply
	}
	switch radius.Code(last[0]) {
	case radius.CodeAccessAccept:
		fmt.Println("accept")
	case radius.CodeAccessReject:
		fmt.Println("reject")
	case radius.CodeAccessChallenge:
		fmt.Println("challenge")
	default:
		fail(fmt.Errorf("unexpected reply code %d", last[0]))
	}
}

func replyMessageAuthenticatorOK(reply, request, secret []byte) bool {
	if len(reply) < 38 || radius.Type(reply[20]) != rfc2869.MessageAuthenticator_Type || reply[21] != 18 {
		return false
	}
	check := append([]byte(nil), reply...)
	copy(check[4:20], request[4:20])
	clear(check[22:38])
	mac := hmac.New(md5.New, secret)
	mac.Write(check)
	return hmac.Equal(mac.Sum(nil), reply[22:38])
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "radius-probe:", err)
	os.Exit(1)
}
