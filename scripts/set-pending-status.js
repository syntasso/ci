// @ts-check
const core = require('@actions/core');
const github = require('@actions/github');

async function run() {
  try {
    const context = 'ci-tests';
    const sha = process.env.GITHUB_SHA;

    const octokit = github.getOctokit(core.getInput('token'));

    await octokit.rest.repos.createCommitStatus({
      owner: 'syntasso',
      repo: 'kratix',
      sha: sha,
      state: 'pending',
      target_url: `https://github.com/syntasso/ci/actions/runs/${github.context.runId}`,
      description: 'CI tests in progress',
      context: context
    });

    core.info('Successfully set commit status to pending');
  } catch (error) {
    core.setFailed(`Action failed with error ${error}`);
  }
}

run();
