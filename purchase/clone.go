package purchase

import "encoding/json"

func cloneOffer(in Offer) Offer {
	out := in
	out.Effects = make([]Effect, len(in.Effects))
	for i, e := range in.Effects {
		out.Effects[i] = e
		if e.Credit != nil {
			v := *e.Credit
			out.Effects[i].Credit = &v
		}
		if e.Plan != nil {
			v := *e.Plan
			out.Effects[i].Plan = &v
		}
		if e.Host != nil {
			v := *e.Host
			v.Payload = append(json.RawMessage(nil), e.Host.Payload...)
			out.Effects[i].Host = &v
		}
		if e.Settlement != nil {
			v := *e.Settlement
			out.Effects[i].Settlement = &v
		}
	}
	return out
}

func clonePrice(in Price) Price { return in }

func cloneQuote(in Quote) Quote {
	out := in
	out.Lines = make([]QuoteLine, len(in.Lines))
	for i, line := range in.Lines {
		out.Lines[i] = line
		out.Lines[i].Offer = cloneOffer(line.Offer)
		out.Lines[i].PriceSnapshot = clonePrice(line.PriceSnapshot)
	}
	return out
}
