package api

func init() {
	// B41.185: PUT …/marketplace/listings/{listingID}/content-labels, GET and PUT …/marketplace/content-filter, and PUT
	// and DELETE …/marketplace/blocked-listings/{listingID} — a listing's age rating and labels and a workspace's filter.
	undocumentedRoutes["B41.185"] = 5
}
