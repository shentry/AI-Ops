import { Activity, ChartColumnBig, LayoutDashboard, LucideIcon, Network, Rocket, ShieldCheck, Siren } from "lucide-react";

export interface NavItem {
  to: string;
  label: string;
  icon: LucideIcon;
  keywords: string;
}

export interface NavGroup {
  label: string;
  items: NavItem[];
}

// 导航只列后端真实提供的页面；侧栏和快速跳转共用这一份。
export const navGroups: NavGroup[] = [
  {
    label: "工作台",
    items: [
      { to: "/", label: "概览", icon: LayoutDashboard, keywords: "overview home dashboard 首页" },
      { to: "/incidents", label: "事件", icon: Siren, keywords: "incident alert 告警 事件" },
      { to: "/monitor", label: "监控", icon: Activity, keywords: "monitor metrics dashboard grafana 监控 看板 指标" },
      { to: "/topology", label: "拓扑", icon: Network, keywords: "topology dependency graph 拓扑 依赖" },
    ],
  },
  {
    label: "自动处置",
    items: [
      { to: "/remediation", label: "处置规则", icon: ShieldCheck, keywords: "remediation rules stop 急停 规则" },
      { to: "/report", label: "效果评估", icon: ChartColumnBig, keywords: "report metrics 报表 效果" },
      { to: "/changes", label: "发布记录", icon: Rocket, keywords: "changes release 发布 回退" },
    ],
  },
];
