package device

import (
	"context"
	"testing"
)

func TestSendSMSIndependentOfMCC(t *testing.T) {
	for _, imsi := range []string{"460001234567890", "461001234567890", "310260123456789"} {
		t.Run(imsi[:3], func(t *testing.T) {
			client := &transcriptClient{
				steps: []clientStep{
					{command: "AT+CMGF=1", response: okResponse()},
					{command: `AT+CSCS="GSM"`, response: okResponse()},
					{command: "AT+CSMP=49,167,0,0", response: okResponse()},
				},
				promptSteps: []promptClientStep{{
					command: `AT+CMGS="+12345"`, payload: "HELLO",
					response: okResponse("+CMGS: 23"),
				}},
			}
			manager, id := newStartedTestManager(t, client)
			injectSnapshot(t, manager, id, &Snapshot{DeviceID: id, IMSI: imsi})
			result, err := manager.SendSMS(context.Background(), id, "+12345", "HELLO")
			if err != nil || !result.AcceptedByModem || result.PartsAccepted != 1 {
				t.Fatalf("SMS = %+v, error = %v", result, err)
			}
			client.assertDone(t)
		})
	}
}
