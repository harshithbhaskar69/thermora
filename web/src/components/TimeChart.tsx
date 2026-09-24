import { useEffect, useRef, useState, type CSSProperties } from "react";
import * as echarts from "echarts/core";
import { LineChart, BarChart } from "echarts/charts";
import {
  GridComponent,
  TooltipComponent,
  DataZoomComponent,
  MarkAreaComponent,
  LegendComponent,
  AriaComponent,
} from "echarts/components";
import { SVGRenderer } from "echarts/renderers";
import type { EChartsOption } from "echarts";
import type { Point } from "./chart-data";
import { timeLabel } from "./chart-data";
import { useColorMode } from "./Theme";

echarts.use([
  LineChart,
  BarChart,
  GridComponent,
  TooltipComponent,
  DataZoomComponent,
  MarkAreaComponent,
  LegendComponent,
  AriaComponent,
  SVGRenderer,
]);

type Series = {
  name: string;
  values: Point[];
  color: "indoor" | "target" | "power" | "outdoor";
  step?: boolean;
  bars?: boolean;
  area?: boolean;
};
export type ChartRow = {
  unit: string;
  series: Series[];
  comfort?: [number, number];
  zero?: boolean;
};

export default function TimeChart({
  rows,
  timezone,
  label,
  live = false,
}: {
  rows: ChartRow[];
  timezone: string;
  label: string;
  live?: boolean;
}) {
  const mode = useColorMode();
  const paletteMode = useRef(mode);
  const host = useRef<HTMLDivElement>(null);
  const instance = useRef<echarts.EChartsType | null>(null);
  const latest = useRef<EChartsOption | null>(null);
  const hovered = useRef(false);
  const zoomed = useRef(false);
  const [inspecting, setInspecting] = useState(false);
  const [hidden, setHidden] = useState<string[]>([]);
  const times = rows.flatMap((row) =>
    row.series.flatMap((series) => series.values.map(([at]) => at)),
  );
  const start = Math.min(...times);
  const end = Math.max(...times);
  const tickTime = new Intl.DateTimeFormat("en-GB", {
    timeZone: timezone,
    hour: "2-digit",
    minute: "2-digit",
  });
  const tickDay = new Intl.DateTimeFormat("en-GB", {
    timeZone: timezone,
    day: "2-digit",
    month: "short",
  });
  const rowHeight = 166;
  const colors = (() => {
    const css = getComputedStyle(document.documentElement);
    return Object.fromEntries(
      [
        "ink",
        "muted",
        "line",
        "surface",
        "indoor",
        "target",
        "power",
        "outdoor",
        "comfort",
      ].map((name) => [name, css.getPropertyValue(`--${name}`).trim()]),
    );
  })();
  const option: EChartsOption = {
    animation: false,
    textStyle: {
      fontFamily: "IBM Plex Sans, sans-serif",
      fontSize: 11,
      color: colors.muted,
    },
    aria: { enabled: true, label: { description: label } },
    legend: { show: false },
    tooltip: {
      trigger: "axis",
      renderMode: "richText",
      confine: true,
      backgroundColor: colors.surface,
      borderColor: colors.line,
      textStyle: { color: colors.ink, fontSize: 12 },
      valueFormatter: (value) =>
        value == null
          ? "Missing"
          : typeof value === "number"
            ? value.toFixed(2)
            : String(value),
    },
    axisPointer: {
      link: [{ xAxisIndex: "all" }],
    },
    grid: rows.map((_, i) => ({
      left: 54,
      right: 12,
      top: i * rowHeight + 26,
      height: 106,
    })),
    xAxis: rows.map((_, i) => ({
      type: "time",
      gridIndex: i,
      min: Number.isFinite(start) ? start : undefined,
      max: Number.isFinite(end) && end > start ? end : undefined,
      axisLabel: {
        color: colors.muted,
        fontSize: 11,
        alignMinLabel: "left",
        alignMaxLabel: "right",
        showMinLabel: true,
        showMaxLabel: true,
        hideOverlap: true,
        formatter: (value: number) =>
          `${end - start > 86_400_000 ? `${tickDay.format(value)}\n` : ""}${tickTime.format(value)}`,
      },
      axisPointer: {
        label: {
          show: true,
          formatter: (params) =>
            timeLabel(Number(params.value), timezone, true),
        },
      },
      axisLine: { lineStyle: { color: colors.line } },
      splitLine: { show: false },
      axisTick: { show: false },
    })),
    yAxis: rows.map((row, i) => ({
      type: "value",
      gridIndex: i,
      name: row.unit,
      nameTextStyle: { align: "left", color: colors.muted },
      scale: !row.zero,
      min: row.comfort
        ? (extent) => Math.floor(Math.min(extent.min, row.comfort![0]) - 0.5)
        : undefined,
      max: row.comfort
        ? (extent) => Math.ceil(Math.max(extent.max, row.comfort![1]) + 0.5)
        : row.unit.endsWith("kW")
          ? (extent) => Math.ceil(extent.max * 1.1)
          : undefined,
      splitNumber: 3,
      axisLabel: { color: colors.muted, fontSize: 10 },
      splitLine: { lineStyle: { color: colors.line } },
    })),
    dataZoom: [
      {
        type: "inside",
        xAxisIndex: rows.map((_, i) => i),
        filterMode: "none",
        zoomOnMouseWheel: "ctrl",
        moveOnMouseWheel: false,
      },
      {
        type: "slider",
        filterMode: "none",
        xAxisIndex: rows.map((_, i) => i),
        bottom: 3,
        height: 18,
        showDetail: false,
        brushSelect: false,
        borderColor: colors.line,
        textStyle: { color: colors.muted },
      },
    ],
    series: rows.flatMap((row, i) =>
      row.series.map((series, j) => ({
        id: series.name,
        name: series.name,
        type: series.bars ? ("bar" as const) : ("line" as const),
        data: series.values,
        xAxisIndex: i,
        yAxisIndex: i,
        showSymbol: false,
        connectNulls: false,
        step: series.step ? ("end" as const) : undefined,
        barMaxWidth: 20,
        itemStyle: { color: colors[series.color] },
        lineStyle: {
          width: 1.8,
          color: colors[series.color],
          type: series.step ? ("dashed" as const) : ("solid" as const),
        },
        areaStyle: series.area
          ? { color: colors[series.color], opacity: 0.1 }
          : undefined,
        markArea:
          j === 0 && row.comfort
            ? {
                silent: true,
                itemStyle: { color: colors.comfort, opacity: 0.65 },
                data: [[{ yAxis: row.comfort[0] }, { yAxis: row.comfort[1] }]],
              }
            : undefined,
      })),
    ),
  };
  useEffect(() => {
    if (!host.current) return;
    const chart = echarts.init(host.current, undefined, { renderer: "svg" });
    instance.current = chart;
    chart.on("datazoom", () => {
      zoomed.current = true;
      setInspecting(true);
    });
    const observer = new ResizeObserver(() => chart.resize());
    observer.observe(host.current);
    return () => {
      observer.disconnect();
      chart.dispose();
      instance.current = null;
    };
  }, []);
  useEffect(() => {
    latest.current = option;
    // Keep inspected evidence still; incoming samples wait for return-to-live.
    if (!hovered.current && !zoomed.current)
      instance.current?.setOption(option, { replaceMerge: ["series"] });
    else if (paletteMode.current !== mode) {
      // Recolor the inspected plot without replacing its data or zoom window.
      instance.current?.setOption({
        textStyle: option.textStyle,
        tooltip: option.tooltip,
        xAxis: rows.map(() => ({
          axisLabel: { color: colors.muted },
          axisLine: { lineStyle: { color: colors.line } },
        })),
        yAxis: rows.map(() => ({
          axisLabel: { color: colors.muted },
          nameTextStyle: { color: colors.muted },
          splitLine: { lineStyle: { color: colors.line } },
        })),
        series: rows.flatMap((row) =>
          row.series.map((series) => ({
            id: series.name,
            itemStyle: { color: colors[series.color] },
            lineStyle: { color: colors[series.color] },
            areaStyle: series.area
              ? { color: colors[series.color], opacity: 0.1 }
              : undefined,
            markArea: { itemStyle: { color: colors.comfort } },
          })),
        ),
      });
    }
    paletteMode.current = mode;
  });
  const resume = () => {
    zoomed.current = false;
    setInspecting(false);
    if (latest.current)
      instance.current?.setOption({
        ...latest.current,
        dataZoom: [
          { start: 0, end: 100 },
          { start: 0, end: 100 },
        ],
      });
  };
  return (
    <>
      <div className="chart-toolbar" aria-label="Chart series">
        {rows
          .flatMap((row) => row.series)
          .map((series) => (
            <label
              key={series.name}
              style={
                { "--series-color": colors[series.color] } as CSSProperties
              }
            >
              <input
                type="checkbox"
                checked={!hidden.includes(series.name)}
                onChange={() => {
                  setHidden((current) =>
                    current.includes(series.name)
                      ? current.filter((name) => name !== series.name)
                      : [...current, series.name],
                  );
                  instance.current?.dispatchAction({
                    type: "legendToggleSelect",
                    name: series.name,
                  });
                }}
              />
              {series.name}
            </label>
          ))}
        {rows.some((row) => row.comfort) && (
          <span className="chart-band-key">
            <span aria-hidden="true" />
            Comfort range
          </span>
        )}
        <div className="chart-actions">
          <button type="button" className="text-button" onClick={resume}>
            {inspecting && live ? "Return to live" : "Reset zoom"}
          </button>
          <button
            type="button"
            className="text-button"
            onClick={() =>
              instance.current?.dispatchAction({
                type: "dataZoom",
                start: 75,
                end: 100,
              })
            }
          >
            Zoom in
          </button>
        </div>
      </div>
      <div
        className="chart"
        ref={host}
        style={{ height: rows.length * rowHeight + 36 }}
        role="img"
        aria-label={`${label} Exact values are available in the data table.`}
        onPointerEnter={() => {
          hovered.current = true;
        }}
        onPointerLeave={() => {
          hovered.current = false;
          if (!zoomed.current && latest.current && instance.current)
            instance.current.setOption(latest.current, {
              replaceMerge: ["series"],
            });
        }}
      />
      <p className="chart-caption">
        {inspecting
          ? `Inspecting a fixed window. ${live ? "Return to live for new samples." : "Reset zoom to see the full window."}`
          : "Drag the time range to inspect. Ctrl + scroll to zoom."}{" "}
        · {timezone}
      </p>
    </>
  );
}
