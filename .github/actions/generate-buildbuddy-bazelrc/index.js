const fs = require('fs');
const path = require('path');

function getInput(name, required = false) {
  const val = process.env[`INPUT_${name.replace(/ /g, '_').toUpperCase()}`] || '';
  if (required && !val) {
    throw new Error(`Input required and not supplied: ${name}`);
  }
  return val.trim();
}

function setFailed(message) {
  console.error(`::error::${message}`);
  process.exit(1);
}

function main() {
  try {
    const templatePath = getInput('template-path', true);
    const outputPath = getInput('output-path', true);
    const apiKey = getInput('api-key', true);

    console.log(`Reading template from: ${templatePath}`);

    // Read the template file
    let content;
    try {
      content = fs.readFileSync(templatePath, 'utf8');
    } catch (error) {
      setFailed(`Failed to read template file: ${error.message}`);
      return;
    }

    console.log('Injecting API key and platform-specific configuration into template');

    // Replace the placeholder with the actual API key
    let generatedContent = content.replace(/BUILDBUDDY_API_KEY/g, apiKey);

    // Add build metadata from GitHub Actions environment variables
    const metadata = [];

    // Repository URL
    if (process.env.GITHUB_REPOSITORY) {
      const repoUrl = `https://github.com/${process.env.GITHUB_REPOSITORY}.git`;
      metadata.push(`common --build_metadata=REPO_URL=${repoUrl}`);
    }

    // Branch name - prefer GITHUB_HEAD_REF for PRs, fallback to GITHUB_REF
    let branchName = process.env.GITHUB_HEAD_REF;
    if (!branchName && process.env.GITHUB_REF) {
      // Extract branch name from refs/heads/branch-name
      branchName = process.env.GITHUB_REF.replace(/^refs\/heads\//, '');
    }
    if (branchName) {
      metadata.push(`common --build_metadata=BRANCH_NAME=${branchName}`);
    }

    // Commit SHA
    if (process.env.GITHUB_SHA) {
      metadata.push(`common --build_metadata=COMMIT_SHA=${process.env.GITHUB_SHA}`);
    }

    // User/Actor
    if (process.env.GITHUB_ACTOR) {
      metadata.push(`common --build_metadata=USER=${process.env.GITHUB_ACTOR}`);
    }

    // Append metadata to the generated content
    if (metadata.length > 0) {
      console.log('Adding build metadata from GitHub Actions context');
      generatedContent += '\n# Metadata from GitHub Actions context\n';
      generatedContent += metadata.join('\n') + '\n';
    }

    // Ensure the output directory exists
    const outputDir = path.dirname(outputPath);
    if (!fs.existsSync(outputDir)) {
      fs.mkdirSync(outputDir, { recursive: true });
    }

    console.log(`Writing generated bazelrc to: ${outputPath}`);

    // Write the generated file
    try {
      fs.writeFileSync(outputPath, generatedContent, 'utf8');
    } catch (error) {
      setFailed(`Failed to write output file: ${error.message}`);
      return;
    }

    console.log('Successfully generated BuildBuddy bazelrc file');
  } catch (error) {
    setFailed(`Action failed with error: ${error.message}`);
  }
}

main();
