package hivestack

import (
	"encoding/json"
	"fmt"
	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v2/adapters"
	"github.com/prebid/prebid-server/v2/config"
	"github.com/prebid/prebid-server/v2/errortypes"
	"github.com/prebid/prebid-server/v2/openrtb_ext"
	"github.com/prebid/prebid-server/v2/util/uuidutil"
	"net/http"
	"regexp"
	"strings"
)

type HivestackAdapter struct {
	endpoint string
}
type hivestackResponseElement struct {
	ImpressionURL      string   `json:"impression_url"`
	ErrorURL           string   `json:"error_url"`
	CPM                float64  `json:"cpm"`
	PurchaseType       string   `json:"purchase_type"`
	RevenueSource      string   `json:"revenue_source"`
	DealID             string   `json:"deal_id"`
	DSP                uint64   `json:"dsp"`
	DSPName            string   `json:"dsp_name"`
	Advertiser         uint64   `json:"advertiser"`
	AdvertiserDomains  []string `json:"advertiser_domains"`
	AdvertiserName     string   `json:"advertiser_name"`
	ImpressionCount    float64  `json:"impression_count"`
	CreativeURL        string   `json:"creative_url"`
	CreativeID         string   `json:"creative_id"`
	CreativeWidth      int64    `json:"creative_width"`
	CreativeHeight     int64    `json:"creative_height"`
	CreativeProperties []string `json:"creative_properties"`
	Duration           uint64   `json:"duration"`
	MIMEType           string   `json:"mime_type"`
	ContentCategories  []string `json:"content_categories"`
}
type hivestackResponse []hivestackResponseElement

var bidUUIDRegex = regexp.MustCompile("/reportvast/([a-f0-9-]{36})")
var uuidGenerator = uuidutil.UUIDRandomGenerator{}

const unexpectedStatusCodeFormat = "Unexpected status code: %d. Run with request.debug = 1 for more info"
const unsupportedMultiResponseFormat = "Unsupported multi-response.  Run with request.debug = 1 for more info"

// Builder builds a new instance of the Hivestack adapter for the given bidder with the given config.
func Builder(bidderName openrtb_ext.BidderName, config config.Adapter, server config.Server) (adapters.Bidder, error) {
	bidder := &HivestackAdapter{
		endpoint: config.Endpoint,
	}
	return bidder, nil
}

func (adapter *HivestackAdapter) makeRequestHeaders(req *openrtb2.BidRequest) http.Header {
	headers := make(http.Header)
	headers.Add("User-Agent", "Prebid-Server-Go/Adapters/Hivestack")
	if req.Device.IP != "" {
		headers.Add("X-Forwarded-For", req.Device.IP)
	} else if req.Device.IPv6 != "" {
		headers.Add("X-Forwarded-For", req.Device.IPv6)
	}
	if req.Device.UA != "" {
		headers.Add("X-Forwarded-User-Agent", req.Device.UA)
	}
	return headers
}

// MakeRequests prepares the HTTP requests which should be made to fetch bids.
func (adapter *HivestackAdapter) MakeRequests(
	request *openrtb2.BidRequest,
	reqInfo *adapters.ExtraRequestInfo,
) (
	requestsToBidder []*adapters.RequestData,
	errs []error,
) {

	var err error

	for _, imp := range request.Imp {

		var bidderExt adapters.ExtImpBidder
		if err = json.Unmarshal(imp.Ext, &bidderExt); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: err.Error(),
			})
			continue
		}

		var hs openrtb_ext.ExtImpHivestack
		if err = json.Unmarshal(bidderExt.Bidder, &hs); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: err.Error(),
			})
			continue
		}

		// Assured by framework UnitUUID passed uuid format validation
		uri := adapter.endpoint + "/units/" + hs.UnitUUID + "/schedulejson"
		headers := adapter.makeRequestHeaders(request)
		requestToBidder := &adapters.RequestData{
			Method:  "GET",
			Uri:     uri,
			ImpIDs:  []string{imp.ID},
			Headers: headers,
		}
		requestsToBidder = append(requestsToBidder, requestToBidder)
	}

	return requestsToBidder, errs
}

// MakeBids unpacks the server's response into Bids.
func (adapter *HivestackAdapter) MakeBids(
	request *openrtb2.BidRequest,
	requestToBidder *adapters.RequestData,
	bidderRawResponse *adapters.ResponseData,
) (
	bidderResponse *adapters.BidderResponse,
	errs []error,
) {

	// Basic response/header/body validation
	contentType := bidderRawResponse.Headers.Get("Content-Type")
	// Get left of ; in case of: application/json; charset=utf-8
	contentType = strings.SplitN(contentType, ";", 2)[0]
	if bidderRawResponse.StatusCode == http.StatusOK && contentType == "application/json" {
		// Happy path, proceed
	} else if bidderRawResponse.StatusCode == http.StatusNoContent {
		return nil, nil
	} else if bidderRawResponse.StatusCode == http.StatusBadRequest {
		// Problem on our side
		err := &errortypes.BadInput{
			Message: fmt.Sprintf(unexpectedStatusCodeFormat, bidderRawResponse.StatusCode),
		}
		return nil, []error{err}
	} else {
		err := &errortypes.BadServerResponse{
			Message: fmt.Sprintf(unexpectedStatusCodeFormat, bidderRawResponse.StatusCode),
		}
		return nil, []error{err}
	}

	// Parse and validate logical shape
	var response hivestackResponse
	if err := json.Unmarshal(bidderRawResponse.Body, &response); err != nil {
		return nil, []error{err}
	} else if len(response) == 0 {
		// Same as no-content
		return nil, nil
	} else if len(response) > 1 {
		// For now:
		err := &errortypes.BadServerResponse{
			Message: fmt.Sprintf(unsupportedMultiResponseFormat),
		}
		return nil, []error{err}
	}

	bidderResponse = adapters.NewBidderResponseWithBidsCapacity(len(response))
	for _, single := range response {
		single := single // pin! -> https://github.com/kyoh86/scopelint#whats-this
		if tb := adapter.makeSingleBid(&single, requestToBidder); tb != nil {
			bidderResponse.Bids = append(bidderResponse.Bids, tb)
		}
	}

	return bidderResponse, nil

}

func (adapter *HivestackAdapter) makeSingleBid(single *hivestackResponseElement, requestToBidder *adapters.RequestData) (tb *adapters.TypedBid) {

	if single == nil || single.ImpressionURL == "" || single.CreativeURL == "" {
		return nil
	}

	var bidType openrtb_ext.BidType
	if strings.HasPrefix(single.MIMEType, "image/") {
		bidType = openrtb_ext.BidTypeBanner
	} else if strings.HasPrefix(single.MIMEType, "video/") {
		bidType = openrtb_ext.BidTypeVideo
	} else if strings.HasPrefix(single.MIMEType, "audio/") {
		bidType = openrtb_ext.BidTypeAudio
	} else {
		return nil
	}

	// Until it's more canonical in the payload, fish it out:
	bidID := ""
	if m := bidUUIDRegex.FindStringSubmatch(single.ImpressionURL); m != nil {
		bidID = m[1]
	} else if newBidID, err := uuidGenerator.Generate(); err == nil {
		bidID = newBidID
	} else {
		return nil
	}

	impID := requestToBidder.ImpIDs[0]
	// validateBid later will refuse a 0-CPM item without a deal
	if single.CPM == 0 && single.DealID == "" {
		single.DealID = "//adserver//"
	}

	return &adapters.TypedBid{
		BidType: bidType,
		Bid: &openrtb2.Bid{
			ID:      bidID,
			ImpID:   impID,
			Price:   single.CPM,
			DealID:  single.DealID,
			CrID:    single.CreativeID,
			ADomain: single.AdvertiserDomains,
			W:       single.CreativeWidth,
			H:       single.CreativeHeight,
			Cat:     single.ContentCategories,
			IURL:    single.CreativeURL,   // preview image
			LURL:    single.ErrorURL,      // loss notice
			BURL:    single.ImpressionURL, // billing notice URL
			Tactic:  single.PurchaseType,
			AdM:     single.CreativeURL,
		},
	}

}
