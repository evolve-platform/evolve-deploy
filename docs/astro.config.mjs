// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// Served on its own domain, so there is no path prefix and no `base`. The
// CNAME that claims it is in public/ — GitHub Pages reads it out of the
// published artifact, so the domain is set here rather than in repository
// settings that nothing in git records.
export default defineConfig({
	site: 'https://deploy.evolve-platform.com',
	integrations: [
		starlight({
			title: 'evolve-deploy',
			// The mark only. The drawing carries the wordmark too, and next to a
			// header that already says `evolve-deploy` that is the name twice.
			logo: {
				light: './src/assets/mark-light.png',
				dark: './src/assets/mark-dark.png',
				alt: '',
			},
			favicon: '/favicon.png',
			description:
				'Stateless deployments to AWS, GCP, Azure and Kubernetes. ' +
				'It reads a config file, compares it against what is actually running, ' +
				'and rolls out the difference.',
			social: [
				{
					icon: 'github',
					label: 'GitHub',
					href: 'https://github.com/evolve-platform/evolve-deploy',
				},
			],
			editLink: {
				baseUrl:
					'https://github.com/evolve-platform/evolve-deploy/edit/main/docs/',
			},
			lastUpdated: true,
			customCss: ['./src/styles/custom.css'],
			sidebar: [
				{
					label: 'Start here',
					items: [
						{ label: 'Introduction', slug: 'getting-started/introduction' },
						{ label: 'How it works', slug: 'getting-started/how-it-works' },
						{ label: 'Install', slug: 'getting-started/install' },
						{ label: 'Your first deploy', slug: 'getting-started/quickstart' },
					],
				},
				{
					label: 'Configuration',
					items: [
						{ label: 'The config file', slug: 'configuration/config-file' },
						{ label: 'Clouds and targets', slug: 'configuration/clouds' },
						{ label: 'Environment variables', slug: 'configuration/environment' },
						{ label: 'References and secrets', slug: 'configuration/references' },
						{ label: 'Templating', slug: 'configuration/templating' },
					],
				},
				{
					label: 'Deploying',
					items: [
						{ label: 'Commands', slug: 'deploying/commands' },
						{ label: 'Ordering with depends_on', slug: 'deploying/ordering' },
						{ label: 'Hooks', slug: 'deploying/hooks' },
						{ label: 'Actions', slug: 'deploying/actions' },
						{ label: 'Failure and recovery', slug: 'deploying/failure' },
					],
				},
				{
					label: 'Blue-green',
					items: [
						{ label: 'How a release works', slug: 'blue-green/overview' },
						{ label: 'Addressing the staged side', slug: 'blue-green/staged-side' },
						{ label: 'Per cloud', slug: 'blue-green/clouds' },
						{ label: 'Rollback and traffic', slug: 'blue-green/rollback' },
					],
				},
				{
					label: 'CI/CD',
					items: [
						{ label: 'GitHub Actions', slug: 'ci/github-actions' },
						{ label: 'Workflow recipes', slug: 'ci/recipes' },
					],
				},
				{
					label: 'Infrastructure',
					items: [
						{ label: 'What Terraform must do', slug: 'infrastructure/terraform' },
					],
				},
				{
					label: 'Reference',
					items: [
						{ label: 'CLI', slug: 'reference/cli' },
						{ label: 'Config schema', slug: 'reference/config' },
					],
				},
			],
		}),
	],
});
