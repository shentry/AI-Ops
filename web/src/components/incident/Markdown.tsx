import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";

import { cx } from "../ui";

// 模型输出来自不可信证据：不渲染原始 HTML（未启用 rehype-raw），去掉图片，
// 链接一律新窗口打开且不带 opener；危险协议由 react-markdown 的默认 urlTransform 过滤。
export function MarkdownText({ children, className }: { children: string; className?: string }) {
  return (
    <div className={cx("markdown", className)}>
      <Markdown
        remarkPlugins={[remarkGfm]}
        disallowedElements={["img"]}
        unwrapDisallowed
        components={{
          a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noreferrer noopener" />,
        }}
      >
        {children}
      </Markdown>
    </div>
  );
}
