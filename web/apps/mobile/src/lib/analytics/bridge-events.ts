// Bridge event properties are categorical. Wallets, quotes and errors stay in the flow.
export const BRIDGE_EVENTS = {
  receiveOpened: "Bridge Receive Opened",
  receiveRouteSelected: "Bridge Receive Route Selected",
  receiveAddressResult: "Bridge Receive Address Result",
  receiveAddressCopied: "Bridge Receive Address Copied",
  incomingReview: "Bridge Incoming Review",
  incomingSubmit: "Bridge Incoming Submit",
  incomingResult: "Bridge Incoming Result",
  incomingCanceled: "Bridge Incoming Review Canceled",
  outOpened: "Bridge Out Opened",
  outRouteSelected: "Bridge Out Route Selected",
  outRecipientSelected: "Bridge Out Recipient Selected",
  outQuoteRetried: "Bridge Out Quote Retried",
  outSubmit: "Bridge Out Submit",
  outResult: "Bridge Out Result",
  outSettled: "Bridge Out Settled",
} as const;
