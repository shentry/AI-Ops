// 比例在分母为 0 时是 null：小样本不显示成比率，而是「样本不足」。
export function percent(value: number | null): string {
  return value === null ? "样本不足" : `${(value * 100).toFixed(1)}%`;
}

export function minutes(value: number | null): string {
  return value === null ? "样本不足" : `${value.toFixed(1)} 分钟`;
}
