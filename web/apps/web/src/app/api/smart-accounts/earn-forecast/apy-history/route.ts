import { NextResponse } from "next/server";

import { getMediumFeeAwareEarnForecast } from "@/lib/kamino/earn-forecast.server";

export async function GET() {
  const forecast = await getMediumFeeAwareEarnForecast();
  if (!forecast) {
    return NextResponse.json(
      { error: "Earn APY is not measured yet" },
      { status: 503 }
    );
  }

  return NextResponse.json(forecast.history);
}
