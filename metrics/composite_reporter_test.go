package metrics_test

import (
	"code.cloudfoundry.org/gorouter/metrics/reporter"
	"code.cloudfoundry.org/gorouter/metrics/reporter/fakes"
	"code.cloudfoundry.org/routing-api/models"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"net/http"
	"time"

	"code.cloudfoundry.org/gorouter/metrics"
	"code.cloudfoundry.org/gorouter/route"
)

var _ = Describe("CompositeReporter", func() {

	var fakeReporter1 *fakes.FakeProxyReporter
	var fakeReporter2 *fakes.FakeProxyReporter
	var composite reporter.ProxyReporter

	var req *http.Request
	var endpoint *route.Endpoint
	var response *http.Response
	var responseTime time.Time
	var responseDuration time.Duration

	BeforeEach(func() {
		fakeReporter1 = new(fakes.FakeProxyReporter)
		fakeReporter2 = new(fakes.FakeProxyReporter)

		composite = metrics.NewCompositeReporter(fakeReporter1, fakeReporter2)
		req, _ = http.NewRequest("GET", "https://example.com", nil)
		endpoint = route.NewEndpoint("someId", "host", 2222, "privateId", "2", map[string]string{}, 30, "", models.ModificationTag{})
		response = &http.Response{StatusCode: 200}
		responseTime = time.Now()
		responseDuration = time.Second
	})

	It("forwards CaptureBadRequest to both reporters", func() {
		composite.CaptureBadRequest()

		Expect(fakeReporter1.CaptureBadRequestCallCount()).To(Equal(1))
		Expect(fakeReporter2.CaptureBadRequestCallCount()).To(Equal(1))

	})

	It("forwards CaptureBadGateway to both reporters", func() {
		composite.CaptureBadGateway()
		Expect(fakeReporter1.CaptureBadGatewayCallCount()).To(Equal(1))
		Expect(fakeReporter2.CaptureBadGatewayCallCount()).To(Equal(1))

	})

	It("forwards CaptureRoutingRequest to both reporters", func() {
		composite.CaptureRoutingRequest(endpoint)
		Expect(fakeReporter1.CaptureRoutingRequestCallCount()).To(Equal(1))
		Expect(fakeReporter2.CaptureRoutingRequestCallCount()).To(Equal(1))

		callEndpoint := fakeReporter1.CaptureRoutingRequestArgsForCall(0)
		Expect(callEndpoint).To(Equal(endpoint))

		callEndpoint = fakeReporter2.CaptureRoutingRequestArgsForCall(0)
		Expect(callEndpoint).To(Equal(endpoint))
	})

	It("forwards CaptureRoutingResponse to both reporters", func() {
		composite.CaptureRoutingResponse(endpoint, response.StatusCode, responseDuration)

		Expect(fakeReporter1.CaptureRoutingResponseCallCount()).To(Equal(1))
		Expect(fakeReporter2.CaptureRoutingResponseCallCount()).To(Equal(1))

		callEndpoint, callResponseStatus, callDuration := fakeReporter1.CaptureRoutingResponseArgsForCall(0)
		Expect(callEndpoint).To(Equal(endpoint))
		Expect(callResponseStatus).To(Equal(response.StatusCode))
		Expect(callDuration).To(Equal(responseDuration))

		callEndpoint, callResponseStatus, callDuration = fakeReporter2.CaptureRoutingResponseArgsForCall(0)
		Expect(callEndpoint).To(Equal(endpoint))
		Expect(callResponseStatus).To(Equal(response.StatusCode))
		Expect(callDuration).To(Equal(responseDuration))
	})
})
