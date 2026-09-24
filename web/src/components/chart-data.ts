import type { Sample } from "../api/client";

export type Point = [number, number | null];

export function timeLabel(
  at: number | string,
  timezone: string,
  detailed = false,
) {
  return new Intl.DateTimeFormat("en-GB", {
    timeZone: timezone,
    ...(detailed ? ({ day: "2-digit", month: "short" } as const) : {}),
    hour: "2-digit",
    minute: "2-digit",
    timeZoneName: "shortOffset",
  }).format(new Date(at));
}

// Compare capture times: accelerated simulation changes observed-time cadence.
export function samplePoints(
  samples: Sample[],
  value: (sample: Sample) => number | undefined,
): Point[] {
  const ordered = [...samples].sort(
    (a, b) => Date.parse(a.observed_at) - Date.parse(b.observed_at),
  );
  const points: Point[] = [];
  for (const [index, sample] of ordered.entries()) {
    const previous = ordered[index - 1];
    const at = Date.parse(sample.observed_at);
    if (
      previous &&
      (sample.sample_seq !== previous.sample_seq + 1 ||
        sample.quality_flags.includes("gap_before") ||
        Date.parse(sample.recorded_at_wall) -
          Date.parse(previous.recorded_at_wall) >
          15_000)
    ) {
      points.push([(Date.parse(previous.observed_at) + at) / 2, null]);
    }
    const reading = sample.quality_flags.some((flag) =>
      ["sensor_fault", "out_of_range", "missing", "clock_uncertain"].includes(
        flag,
      ),
    )
      ? undefined
      : value(sample);
    points.push([
      at,
      reading !== undefined && Number.isFinite(reading) ? reading : null,
    ]);
  }
  return points;
}

export function historyWindow(
  period: string,
  day: string,
  end: string | undefined,
) {
  if (period === "day" || !end) return { day };
  const hours = period === "week" ? 168 : 24;
  return {
    from: new Date(Date.parse(end) - hours * 3_600_000).toISOString(),
    to: end,
  };
}
