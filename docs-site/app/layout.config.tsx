import type { BaseLayoutProps } from 'fumadocs-ui/layouts/shared';

export const baseOptions: BaseLayoutProps = {
  nav: {
    title: (
      <span className="inline-flex items-center gap-2 font-semibold">
        <img src="/vinted-cli-logo.svg" alt="vinted-cli" width={150} height={50} className="h-8 w-auto" />
      </span>
    ),
  },
  githubUrl: 'https://github.com/Microck/vinted-cli',
};
